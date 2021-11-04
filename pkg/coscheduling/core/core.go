/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	gochache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	informerv1 "k8s.io/client-go/informers/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	pginformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	pglister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// Manager defines the interfaces for PodGroup management.
type Manager interface {
	PreFilter(context.Context, *corev1.Pod) error
	Permit(context.Context, *corev1.Pod, string) (bool, error)
	PostBind(context.Context, *corev1.Pod, string)
	GetPodGroup(*corev1.Pod) (string, *v1alpha1.PodGroup)
	GetCreationTimestamp(*corev1.Pod, time.Time) time.Time
	AddDeniedPodGroup(string)
	DeletePermittedPodGroup(string)
	CalculateAssignedPods(string, string) int
}

// PodGroupManager defines the scheduling operation called
type PodGroupManager struct {
	// pgClient is a podGroup client
	pgClient pgclientset.Interface
	// snapshotSharedLister is pod shared list
	snapshotSharedLister framework.SharedLister
	// scheduleTimeout is the default time when group scheduling.
	// If podgroup's ScheduleTimeoutSeconds set, that would be used.
	scheduleTimeout *time.Duration
	// lastDeniedPG stores the pg name if a pod can not pass pre-filer,
	// or anyone of the pod timeout
	lastDeniedPG *gochache.Cache
	// permittedPG stores the pg name which has passed the pre resource check.
	permittedPG *gochache.Cache
	// deniedCacheExpirationTime is the expiration time that a podGroup remains in lastDeniedPG store.
	lastDeniedPGExpirationTime *time.Duration
	// pgLister is podgroup lister
	pgLister pglister.PodGroupLister
	// podLister is pod lister
	podLister listerv1.PodLister
	// reserveResourcePercentage is the reserved resource for the max finished group, range (0,100]
	reserveResourcePercentage int32
	sync.RWMutex
}

// NewPodGroupManager create a new operation object
func NewPodGroupManager(pgClient pgclientset.Interface, snapshotSharedLister framework.SharedLister, scheduleTimeout, deniedPGExpirationTime *time.Duration,
	pgInformer pginformer.PodGroupInformer, podInformer informerv1.PodInformer) *PodGroupManager {
	pgMgr := &PodGroupManager{
		pgClient:                   pgClient,
		snapshotSharedLister:       snapshotSharedLister,
		scheduleTimeout:            scheduleTimeout,
		lastDeniedPGExpirationTime: deniedPGExpirationTime,
		pgLister:                   pgInformer.Lister(),
		podLister:                  podInformer.Lister(),
		lastDeniedPG:               gochache.New(3*time.Second, 3*time.Second),
		permittedPG:                gochache.New(3*time.Second, 3*time.Second),
	}
	return pgMgr
}

// PreFilter filters out a pod if it
// 1. belongs to a podgroup that was recently denied or
// 2. the total number of pods in the podgroup is less than the minimum number of pods
// that is required to be scheduled.
func (pgMgr *PodGroupManager) PreFilter(ctx context.Context, pod *corev1.Pod) error {
	klog.V(5).InfoS("Pre-filter", "pod", klog.KObj(pod))
	pgFullName, pg := pgMgr.GetPodGroup(pod)
	if pg == nil {
		return nil
	}
	if _, ok := pgMgr.lastDeniedPG.Get(pgFullName); ok {
		return fmt.Errorf("pod with pgName: %v last failed in 3s, deny", pgFullName)
	}
	pods, err := pgMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{util.PodGroupLabel: util.GetPodGroupLabel(pod)}),
	)
	if err != nil {
		return fmt.Errorf("podLister list pods failed: %v", err)
	}
	if len(pods) < int(pg.Spec.MinMember) {
		return fmt.Errorf("pre-filter pod %v cannot find enough sibling pods, "+
			"current pods number: %v, minMember of group: %v", pod.Name, len(pods), pg.Spec.MinMember)
	}

	if pg.Spec.MinResources == nil {
		return nil
	}

	// TODO(cwdsuzhou): This resource check may not always pre-catch unschedulable pod group.
	// It only tries to PreFilter resource constraints so even if a PodGroup passed here,
	// it may not necessarily pass Filter due to other constraints such as affinity/taints.
	if _, ok := pgMgr.permittedPG.Get(pgFullName); ok {
		return nil
	}

	nodes, err := pgMgr.snapshotSharedLister.NodeInfos().List()
	if err != nil {
		return err
	}

	minResources := pg.Spec.MinResources.DeepCopy()
	podQuantity := resource.NewQuantity(int64(pg.Spec.MinMember), resource.DecimalSI)
	minResources[corev1.ResourcePods] = *podQuantity
	err = CheckClusterResource(nodes, minResources, pgFullName)
	if err != nil {
		klog.ErrorS(err, "Failed to PreFilter", "podGroup", klog.KObj(pg))
		pgMgr.AddDeniedPodGroup(pgFullName)
		return err
	}
	pgMgr.permittedPG.Add(pgFullName, pgFullName, *pgMgr.scheduleTimeout)
	return nil
}

// Permit permits a pod to run, if the minMember match, it would send a signal to chan.
func (pgMgr *PodGroupManager) Permit(ctx context.Context, pod *corev1.Pod, nodeName string) (bool, error) {
	pgFullName, pg := pgMgr.GetPodGroup(pod)
	if pgFullName == "" {
		return true, util.ErrorNotMatched
	}
	if pg == nil {
		// A Pod with a podGroup name but without a PodGroup found is denied.
		return false, fmt.Errorf("PodGroup not found")
	}

	assigned := pgMgr.CalculateAssignedPods(pg.Name, pg.Namespace)
	// The number of pods that have been assigned nodes is calculated from the snapshot.
	// The current pod in not included in the snapshot during the current scheduling cycle.
	ready := int32(assigned)+1 >= pg.Spec.MinMember
	if ready {
		return true, nil
	}
	return false, util.ErrorWaiting
}

// PostBind updates a PodGroup's status.
func (pgMgr *PodGroupManager) PostBind(ctx context.Context, pod *corev1.Pod, nodeName string) {
	pgMgr.Lock()
	defer pgMgr.Unlock()
	pgFullName, pg := pgMgr.GetPodGroup(pod)
	if pgFullName == "" || pg == nil {
		return
	}
	pgCopy := pg.DeepCopy()
	pgCopy.Status.Scheduled++

	if pgCopy.Status.Scheduled >= pgCopy.Spec.MinMember {
		pgCopy.Status.Phase = v1alpha1.PodGroupScheduled
	} else {
		pgCopy.Status.Phase = v1alpha1.PodGroupScheduling
		if pgCopy.Status.ScheduleStartTime.IsZero() {
			pgCopy.Status.ScheduleStartTime = metav1.Time{Time: time.Now()}
		}
	}
	if pgCopy.Status.Phase != pg.Status.Phase {
		pg, err := pgMgr.pgLister.PodGroups(pgCopy.Namespace).Get(pgCopy.Name)
		if err != nil {
			klog.ErrorS(err, "Failed to get PodGroup", "podGroup", klog.KObj(pgCopy))
			return
		}
		patch, err := util.CreateMergePatch(pg, pgCopy)
		if err != nil {
			klog.ErrorS(err, "Failed to create merge patch", "podGroup", klog.KObj(pg), "podGroup", klog.KObj(pgCopy))
			return
		}
		if err := pgMgr.PatchPodGroup(pg.Name, pg.Namespace, patch); err != nil {
			klog.ErrorS(err, "Failed to patch", "podGroup", klog.KObj(pg))
			return
		}
		pg.Status.Phase = pgCopy.Status.Phase
	}
	pg.Status.Scheduled = pgCopy.Status.Scheduled
	return
}

// GetCreationTimestamp returns the creation time of a podGroup or a pod.
func (pgMgr *PodGroupManager) GetCreationTimestamp(pod *corev1.Pod, ts time.Time) time.Time {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return ts
	}
	pg, err := pgMgr.pgLister.PodGroups(pod.Namespace).Get(pgName)
	if err != nil {
		return ts
	}
	return pg.CreationTimestamp.Time
}

// AddDeniedPodGroup adds a podGroup that fails to be scheduled to a PodGroup cache with expriration.
func (pgMgr *PodGroupManager) AddDeniedPodGroup(pgFullName string) {
	pgMgr.lastDeniedPG.Add(pgFullName, "", *pgMgr.lastDeniedPGExpirationTime)
}

// DeletePodGroup delete a podGroup that pass Pre-Filter but reach PostFilter.
func (pgMgr *PodGroupManager) DeletePermittedPodGroup(pgFullName string) {
	pgMgr.permittedPG.Delete(pgFullName)
}

// PatchPodGroup patches a podGroup.
func (pgMgr *PodGroupManager) PatchPodGroup(pgName string, namespace string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	_, err := pgMgr.pgClient.SchedulingV1alpha1().PodGroups(namespace).Patch(context.TODO(), pgName,
		types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// GetPodGroup returns the PodGroup that a Pod belongs to from cache.
func (pgMgr *PodGroupManager) GetPodGroup(pod *corev1.Pod) (string, *v1alpha1.PodGroup) {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return "", nil
	}
	pg, err := pgMgr.pgLister.PodGroups(pod.Namespace).Get(pgName)
	if err != nil {
		return fmt.Sprintf("%v/%v", pod.Namespace, pgName), nil
	}
	return fmt.Sprintf("%v/%v", pod.Namespace, pgName), pg
}

// CalculateAssignedPods returns the number of pods that has been assigned a node: assumed or bound.
func (pgMgr *PodGroupManager) CalculateAssignedPods(podGroupName, namespace string) int {
	nodeInfos, err := pgMgr.snapshotSharedLister.NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "Cannot get nodeInfos from frameworkHandle")
		return 0
	}
	var count int
	for _, nodeInfo := range nodeInfos {
		for _, podInfo := range nodeInfo.Pods {
			pod := podInfo.Pod
			if pod.Labels[util.PodGroupLabel] == podGroupName && pod.Namespace == namespace && pod.Spec.NodeName != "" {
				count++
			}
		}
	}

	return count
}

// CheckClusterResource checks if resource capacity of the cluster can satisfy <resourceRequest>.
// It returns an error detailing the resource gap if not satisfied; otherwise returns nil.
func CheckClusterResource(nodeList []*framework.NodeInfo, resourceRequest corev1.ResourceList, desiredPodGroupName string) error {
	for _, info := range nodeList {
		if info == nil || info.Node() == nil {
			continue
		}

		nodeResource := getNodeResource(info, desiredPodGroupName).ResourceList()
		for name, quant := range resourceRequest {
			quant.Sub(nodeResource[name])
			if quant.Sign() <= 0 {
				delete(resourceRequest, name)
				continue
			}
			resourceRequest[name] = quant
		}
		if len(resourceRequest) == 0 {
			return nil
		}
	}
	return fmt.Errorf("resource gap: %v", resourceRequest)
}

// GetNamespacedName returns the namespaced name
func GetNamespacedName(obj metav1.Object) string {
	return fmt.Sprintf("%v/%v", obj.GetNamespace(), obj.GetName())
}

func getNodeResource(info *framework.NodeInfo, desiredPodGroupName string) *framework.Resource {
	nodeClone := info.Clone()
	for _, podInfo := range info.Pods {
		if podInfo == nil || podInfo.Pod == nil {
			continue
		}
		if util.GetPodGroupFullName(podInfo.Pod) != desiredPodGroupName {
			continue
		}
		nodeClone.RemovePod(podInfo.Pod)
	}

	leftResource := framework.Resource{
		ScalarResources: make(map[corev1.ResourceName]int64),
	}
	allocatable := nodeClone.Allocatable
	requested := nodeClone.Requested

	leftResource.AllowedPodNumber = allocatable.AllowedPodNumber - len(nodeClone.Pods)
	leftResource.MilliCPU = allocatable.MilliCPU - requested.MilliCPU
	leftResource.Memory = allocatable.Memory - requested.Memory
	leftResource.EphemeralStorage = allocatable.EphemeralStorage - requested.EphemeralStorage

	for k, allocatableEx := range allocatable.ScalarResources {
		requestEx, ok := requested.ScalarResources[k]
		if !ok {
			leftResource.ScalarResources[k] = allocatableEx
		} else {
			leftResource.ScalarResources[k] = allocatableEx - requestEx
		}
	}
	klog.V(4).InfoS("Node left resource", "node", klog.KObj(info.Node()), "resource", leftResource)
	return &leftResource
}
