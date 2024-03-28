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

	gocache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	informerv1 "k8s.io/client-go/informers/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

type Status string

const (
	// PodGroupNotSpecified denotes no PodGroup is specified in the Pod spec.
	PodGroupNotSpecified Status = "PodGroup not specified"
	// PodGroupNotFound denotes the specified PodGroup in the Pod spec is
	// not found in API server.
	PodGroupNotFound Status = "PodGroup not found"
	Success          Status = "Success"
	Wait             Status = "Wait"

	permitStateKey = "PermitCoscheduling"
)

type PermitState struct {
	Activate bool
}

func (s *PermitState) Clone() framework.StateData {
	return &PermitState{Activate: s.Activate}
}

// Manager defines the interfaces for PodGroup management.
type Manager interface {
	PreFilter(context.Context, *corev1.Pod) error
	Permit(context.Context, *framework.CycleState, *corev1.Pod) Status
	Unreserve(context.Context, *corev1.Pod)
	GetPodGroup(context.Context, *corev1.Pod) (string, *v1alpha1.PodGroup)
	GetAssignedPodCount(string) int
	GetCreationTimestamp(context.Context, *corev1.Pod, time.Time) time.Time
	DeletePermittedPodGroup(context.Context, string)
	ActivateSiblings(ctx context.Context, pod *corev1.Pod, state *framework.CycleState)
	BackoffPodGroup(string, time.Duration)
}

// PodGroupManager defines the scheduling operation called
type PodGroupManager struct {
	// client is a generic controller-runtime client to manipulate both core resources and PodGroups.
	client client.Client
	// snapshotSharedLister is pod shared list
	snapshotSharedLister framework.SharedLister
	// scheduleTimeout is the default timeout for podgroup scheduling.
	// If podgroup's scheduleTimeoutSeconds is set, it will be used.
	scheduleTimeout *time.Duration
	// permittedPG stores the podgroup name which has passed the pre resource check.
	permittedPG *gocache.Cache
	// backedOffPG stores the podgorup name which failed scheudling recently.
	backedOffPG *gocache.Cache
	// podLister is pod lister
	podLister listerv1.PodLister
	// assignedPodsByPG stores the pods assumed or bound for podgroups
	assignedPodsByPG map[string]sets.Set[string]
	sync.RWMutex
}

func AddPodFactory(pgMgr *PodGroupManager) func(obj interface{}) {
	return func(obj interface{}) {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			return
		}
		if p.Spec.NodeName == "" {
			return
		}
		pgFullName, _ := pgMgr.GetPodGroup(context.Background(), p)
		if pgFullName == "" {
			return
		}
		pgMgr.RWMutex.Lock()
		defer pgMgr.RWMutex.Unlock()
		if assigned, exist := pgMgr.assignedPodsByPG[pgFullName]; exist {
			assigned.Insert(p.Name)
		} else {
			pgMgr.assignedPodsByPG[pgFullName] = sets.New(p.Name)
		}
	}
}

// NewPodGroupManager creates a new operation object.
func NewPodGroupManager(client client.Client, snapshotSharedLister framework.SharedLister, scheduleTimeout *time.Duration, podInformer informerv1.PodInformer) *PodGroupManager {
	pgMgr := &PodGroupManager{
		client:               client,
		snapshotSharedLister: snapshotSharedLister,
		scheduleTimeout:      scheduleTimeout,
		podLister:            podInformer.Lister(),
		permittedPG:          gocache.New(3*time.Second, 3*time.Second),
		backedOffPG:          gocache.New(10*time.Second, 10*time.Second),
		assignedPodsByPG:     map[string]sets.Set[string]{},
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: AddPodFactory(pgMgr),
		DeleteFunc: func(obj interface{}) {
			switch t := obj.(type) {
			case *corev1.Pod:
				pod := t
				if pod.Spec.NodeName == "" {
					return
				}
				pgMgr.Unreserve(context.Background(), pod)
				return
			case cache.DeletedFinalStateUnknown:
				pod, ok := t.Obj.(*corev1.Pod)
				if !ok {
					return
				}
				if pod.Spec.NodeName == "" {
					return
				}
				pgMgr.Unreserve(context.Background(), pod)
				return
			default:
				return
			}
		},
	})
	return pgMgr
}

func (pgMgr *PodGroupManager) GetAssignedPodCount(pgName string) int {
	pgMgr.RWMutex.RLock()
	defer pgMgr.RWMutex.RUnlock()
	return len(pgMgr.assignedPodsByPG[pgName])
}

func (pgMgr *PodGroupManager) BackoffPodGroup(pgName string, backoff time.Duration) {
	if backoff == time.Duration(0) {
		return
	}
	pgMgr.backedOffPG.Add(pgName, nil, backoff)
}

// ActivateSiblings stashes the pods belonging to the same PodGroup of the given pod
// in the given state, with a reserved key "kubernetes.io/pods-to-activate".
func (pgMgr *PodGroupManager) ActivateSiblings(ctx context.Context, pod *corev1.Pod, state *framework.CycleState) {
	lh := klog.FromContext(ctx)
	pgName := util.GetPodGroupLabel(pod)
	if pgName == "" {
		return
	}

	// Only proceed if it's explicitly requested to activate sibling pods.
	if c, err := state.Read(permitStateKey); err != nil {
		return
	} else if s, ok := c.(*PermitState); !ok || !s.Activate {
		return
	}

	pods, err := pgMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{v1alpha1.PodGroupLabel: pgName}),
	)
	if err != nil {
		lh.Error(err, "Failed to obtain pods belong to a PodGroup", "podGroup", pgName)
		return
	}

	for i := range pods {
		if pods[i].UID == pod.UID {
			pods = append(pods[:i], pods[i+1:]...)
			break
		}
	}

	if len(pods) != 0 {
		if c, err := state.Read(framework.PodsToActivateKey); err == nil {
			if s, ok := c.(*framework.PodsToActivate); ok {
				s.Lock()
				for _, pod := range pods {
					namespacedName := GetNamespacedName(pod)
					s.Map[namespacedName] = pod
				}
				s.Unlock()
			}
		}
	}
}

// PreFilter filters out a pod if
// 1. it belongs to a podgroup that was recently denied or
// 2. the total number of pods in the podgroup is less than the minimum number of pods
// that is required to be scheduled.
func (pgMgr *PodGroupManager) PreFilter(ctx context.Context, pod *corev1.Pod) error {
	lh := klog.FromContext(ctx)
	lh.V(5).Info("Pre-filter", "pod", klog.KObj(pod))
	pgFullName, pg := pgMgr.GetPodGroup(ctx, pod)
	if pg == nil {
		return nil
	}

	if _, exist := pgMgr.backedOffPG.Get(pgFullName); exist {
		return fmt.Errorf("podGroup %v failed recently", pgFullName)
	}

	pods, err := pgMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{v1alpha1.PodGroupLabel: util.GetPodGroupLabel(pod)}),
	)
	if err != nil {
		return fmt.Errorf("podLister list pods failed: %w", err)
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
	err = CheckClusterResource(ctx, nodes, minResources, pgFullName)
	if err != nil {
		lh.Error(err, "Failed to PreFilter", "podGroup", klog.KObj(pg))
		return err
	}
	pgMgr.permittedPG.Add(pgFullName, pgFullName, *pgMgr.scheduleTimeout)
	return nil
}

// Permit permits a pod to run, if the minMember match, it would send a signal to chan.
func (pgMgr *PodGroupManager) Permit(ctx context.Context, state *framework.CycleState, pod *corev1.Pod) Status {
	pgFullName, pg := pgMgr.GetPodGroup(ctx, pod)
	if pgFullName == "" {
		return PodGroupNotSpecified
	}
	if pg == nil {
		// A Pod with a podGroup name but without a PodGroup found is denied.
		return PodGroupNotFound
	}

	pgMgr.RWMutex.RLock()
	defer pgMgr.RWMutex.RUnlock()
	assigned, exist := pgMgr.assignedPodsByPG[pgFullName]
	if !exist {
		assigned = sets.Set[string]{}
		pgMgr.assignedPodsByPG[pgFullName] = assigned
	}
	assigned.Insert(pod.Name)
	// The number of pods that have been assigned nodes is calculated from the snapshot.
	// The current pod in not included in the snapshot during the current scheduling cycle.
	if len(assigned) >= int(pg.Spec.MinMember) {
		return Success
	}

	if len(assigned) == 1 {
		// Given we've reached Permit(), it's mean all PreFilter checks (minMember & minResource)
		// already pass through, so if len(assigned) == 1, it could be due to:
		// - minResource get satisfied
		// - new pods added
		// In either case, we should and only should use this 0-th pod to trigger activating
		// its siblings.
		// It'd be in-efficient if we trigger activating siblings unconditionally.
		// See https://github.com/kubernetes-sigs/scheduler-plugins/issues/682
		state.Write(permitStateKey, &PermitState{Activate: true})
	}

	return Wait
}

// Unreserve invalidates assigned pod from assignedPodsByPG when schedule or bind failed.
func (pgMgr *PodGroupManager) Unreserve(ctx context.Context, pod *corev1.Pod) {
	pgFullName, _ := pgMgr.GetPodGroup(ctx, pod)
	if pgFullName == "" {
		return
	}

	pgMgr.RWMutex.Lock()
	defer pgMgr.RWMutex.Unlock()
	assigned, exist := pgMgr.assignedPodsByPG[pgFullName]
	if exist {
		assigned.Delete(pod.Name)
		if len(assigned) == 0 {
			delete(pgMgr.assignedPodsByPG, pgFullName)
		}
	}
}

// GetCreationTimestamp returns the creation time of a podGroup or a pod.
func (pgMgr *PodGroupManager) GetCreationTimestamp(ctx context.Context, pod *corev1.Pod, ts time.Time) time.Time {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return ts
	}
	var pg v1alpha1.PodGroup
	if err := pgMgr.client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pgName}, &pg); err != nil {
		return ts
	}
	return pg.CreationTimestamp.Time
}

// DeletePermittedPodGroup deletes a podGroup that passes Pre-Filter but reaches PostFilter.
func (pgMgr *PodGroupManager) DeletePermittedPodGroup(_ context.Context, pgFullName string) {
	pgMgr.permittedPG.Delete(pgFullName)
}

// GetPodGroup returns the PodGroup that a Pod belongs to in cache.
func (pgMgr *PodGroupManager) GetPodGroup(ctx context.Context, pod *corev1.Pod) (string, *v1alpha1.PodGroup) {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return "", nil
	}
	var pg v1alpha1.PodGroup
	if err := pgMgr.client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pgName}, &pg); err != nil {
		return fmt.Sprintf("%v/%v", pod.Namespace, pgName), nil
	}
	return fmt.Sprintf("%v/%v", pod.Namespace, pgName), &pg
}

// CheckClusterResource checks if resource capacity of the cluster can satisfy <resourceRequest>.
// It returns an error detailing the resource gap if not satisfied; otherwise returns nil.
func CheckClusterResource(ctx context.Context, nodeList []*framework.NodeInfo, resourceRequest corev1.ResourceList, desiredPodGroupName string) error {
	for _, info := range nodeList {
		if info == nil || info.Node() == nil {
			continue
		}

		nodeResource := util.ResourceList(getNodeResource(ctx, info, desiredPodGroupName))
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

// GetNamespacedName returns the namespaced name.
func GetNamespacedName(obj metav1.Object) string {
	return fmt.Sprintf("%v/%v", obj.GetNamespace(), obj.GetName())
}

func getNodeResource(ctx context.Context, info *framework.NodeInfo, desiredPodGroupName string) *framework.Resource {
	nodeClone := info.Snapshot()
	logger := klog.FromContext(ctx)
	for _, podInfo := range info.Pods {
		if podInfo == nil || podInfo.Pod == nil {
			continue
		}
		if util.GetPodGroupFullName(podInfo.Pod) != desiredPodGroupName {
			continue
		}
		nodeClone.RemovePod(logger, podInfo.Pod)
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
	logger.V(4).Info("Node left resource", "node", klog.KObj(info.Node()), "resource", leftResource)
	return &leftResource
}
