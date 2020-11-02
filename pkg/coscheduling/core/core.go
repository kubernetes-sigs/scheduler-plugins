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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	informerv1 "k8s.io/client-go/informers/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	pginformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	pglister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

type Manager interface {
	PreFilter(context.Context, *corev1.Pod) error
	Permit(context.Context, *corev1.Pod, string) (bool, error)
	PostBind(context.Context, *corev1.Pod, string)
	GetPodGroup(*corev1.Pod) (string, *v1alpha1.PodGroup)
	GetCreationTime(*corev1.Pod, time.Time) time.Time
	AddToDenyCache(string)
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
	// lastDeniedPG store the pg name if a pod can not pass pre-filer,
	// or anyone of the pod timeout
	lastDeniedPG *gochache.Cache
	// pgLister is podgroup lister
	pgLister pglister.PodGroupLister
	// podLister is pod lister
	podLister listerv1.PodLister
	// reserveResourcePercentage is the reserved resource for the max finished group, range (0,100]
	reserveResourcePercentage int32
	sync.RWMutex
}

// NewPodGroupManager create a new operation object
func NewPodGroupManager(pgClient pgclientset.Interface, snapshotSharedLister framework.SharedLister, scheduleTimeout *time.Duration,
	pgInformer pginformer.PodGroupInformer, podInformer informerv1.PodInformer) *PodGroupManager {
	pgMgr := &PodGroupManager{
		pgClient:             pgClient,
		snapshotSharedLister: snapshotSharedLister,
		scheduleTimeout:      scheduleTimeout,
		pgLister:             pgInformer.Lister(),
		podLister:            podInformer.Lister(),
		lastDeniedPG:         gochache.New(3*time.Second, 3*time.Second),
	}
	return pgMgr
}

// PreFilter pre-filter pod include:
// 1. cluster resource
// 2. last denied pod to increase schedule qps
// 3. check resource for max finished group, evict others if it can not run
// 4. check pod priority, if it can not run, evict other groups
func (pgMgr *PodGroupManager) PreFilter(ctx context.Context, pod *corev1.Pod) error {
	klog.V(10).Infof("Pre-filter %v", pod.Name)
	pgFullName, pg := pgMgr.GetPodGroup(pod)
	if pg == nil {
		return nil
	}
	if _, ok := pgMgr.lastDeniedPG.Get(pgFullName); ok {
		err := fmt.Errorf("pod with pgName: %v last failed in 20s, deny", pgFullName)
		klog.V(6).Info(err)
		return err
	}
	pods, err := pgMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{util.PodGroupLabel: util.GetPodGroupLabel(pod)}),
	)
	if err != nil {
		return fmt.Errorf("podLister list pods failed: %v", err)
	}
	if len(pods) < int(pg.Spec.MinMember) {
		return fmt.Errorf("cannot found engough pods, "+
			"current pods number: %v, minMember of group: %v", len(pods), pg.Spec.MinMember)
	}
	return nil
}

// Permit permits a pod to run, if the minMember match, it would send a signal to chan.
func (pgMgr *PodGroupManager) Permit(ctx context.Context, pod *corev1.Pod, nodeName string) (bool, error) {
	pgFullName, pg := pgMgr.GetPodGroup(pod)
	if pgFullName == "" {
		return true, util.ErrorNotMatched
	}
	if pg == nil {
		// once we have admission check, a Pod targeting non-exisitng PodGroup won't be created.
		// So here it just serves as a sanity check.
		return false, fmt.Errorf("PodGroup not found")
	}

	bound := pgMgr.calculateBoundPods(pg.Name, pg.Namespace)
	// The bound is calculated from the snapshot. The current pod does not exist in the snapshot during this scheduling cycle.
	ready := int32(bound)+1 >= pg.Spec.MinMember
	if ready {
		return true, nil
	}
	return false, util.ErrorWaiting
}

// PostBind is used for send metrics and update to api if all finish
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
	pg.Status = pgCopy.Status
	if pgCopy.Status.Phase != pg.Status.Phase {

		pg, err := pgMgr.pgLister.PodGroups(pgCopy.Namespace).Get(pgCopy.Name)
		if err != nil {
			klog.Error(err)
			return
		}
		patch, err := util.CreateMergePatch(pg, pgCopy)
		if err != nil {
			klog.Error(err)
			return
		}
		if err := pgMgr.PatchPodGroup(pg.Name, pg.Namespace, patch); err != nil {
			klog.Error(err)
			return
		}
		pg.Status.Phase = pgCopy.Status.Phase
	}
	pg.Status.Scheduled = pgCopy.Status.Scheduled
	return
}

// getCreationTime return creation time of podgroup or pod
func (pgMgr *PodGroupManager) GetCreationTime(pod *corev1.Pod, ts time.Time) time.Time {
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

// AddToDenyCache add podGroup to the back list cache
func (pgMgr *PodGroupManager) AddToDenyCache(pgFullName string) {
	pgMgr.lastDeniedPG.Add(pgFullName, "", 3*time.Second)
}

// PatchPodGroup
func (pgMgr *PodGroupManager) PatchPodGroup(pgName string, namespace string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	_, err := pgMgr.pgClient.SchedulingV1alpha1().PodGroups(namespace).Patch(context.TODO(), pgName,
		types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// GetPodGroup get PodGroup from cache
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

// calculateBoundPods returns the count of pods that have occupied resources (including assumed and bound)
func (pgMgr *PodGroupManager) calculateBoundPods(podGroupName, namespace string) int {
	nodeInfos, err := pgMgr.snapshotSharedLister.NodeInfos().List()
	if err != nil {
		klog.Errorf("Cannot get nodeInfos from frameworkHandle: %v", err)
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

// GetNamespacedName returns the namespaced name
func GetNamespacedName(obj metav1.Object) string {
	return fmt.Sprintf("%v/%v", obj.GetNamespace(), obj.GetName())
}
