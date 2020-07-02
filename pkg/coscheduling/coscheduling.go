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

package coscheduling

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/api/v1/pod"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
)

// Coscheduling is a plugin that implements the mechanism of gang scheduling.
type Coscheduling struct {
	frameworkHandle framework.FrameworkHandle
	podLister       corelisters.PodLister
	// key is <namespace>/<PodGroup name> and value is *PodGroupInfo.
	podGroupInfos sync.Map
}

// PodGroupInfo is a wrapper to a PodGroup with additional information.
// TODO: implement a timeout based gc for the PodGroupInfos map.
type PodGroupInfo struct {
	// key is a unique PodGroup ID and currently implemented as <namespace>/<podgroup name>.
	key string
	// name is the PodGroup name and defined through a Pod label.
	// The PodGroup name of a regular pod is the pod name.
	name string
	// priority is the priority of pods in a PodGroup.
	// All pods in a PodGroup should have the same priority.
	priority int32
	// timestamp stores the timestamp of the initialization time of a PodGroup.
	timestamp time.Time
	// minAvailable is the minimum number of pods to be co-scheduled in a PodGroup.
	// All pods in a PodGroup should have the same minAvailable.
	minAvailable int32
}

var _ framework.QueueSortPlugin = &Coscheduling{}
var _ framework.PreFilterPlugin = &Coscheduling{}
var _ framework.PermitPlugin = &Coscheduling{}
var _ framework.UnreservePlugin = &Coscheduling{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = "Coscheduling"
	// PodGroupName is the name of a pod group that defines a coscheduling pod group.
	PodGroupName = "pod-group.scheduling.sigs.k8s.io/name"
	// PodGroupMinAvailable specifies the minimum number of pods to be scheduled together in a pod group.
	PodGroupMinAvailable = "pod-group.scheduling.sigs.k8s.io/min-available"
	// PermitWaitingTime is the wait timeout returned by Permit plugin.
	// TODO make it configurable
	PermitWaitingTime = 1 * time.Second
)

// Name returns name of the plugin. It is used in logs, etc.
func (cs *Coscheduling) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(_ *runtime.Unknown, handle framework.FrameworkHandle) (framework.Plugin, error) {
	podLister := handle.SharedInformerFactory().Core().V1().Pods().Lister()
	return &Coscheduling{frameworkHandle: handle,
		podLister: podLister,
	}, nil
}

// Less are used to sort pods in the scheduling queue.
// 1. Compare the priorities of pods.
// 2. Compare the timestamps of the initialization time of PodGroups.
// 3. Compare the keys of PodGroups.
func (cs *Coscheduling) Less(podInfo1 *framework.PodInfo, podInfo2 *framework.PodInfo) bool {
	pgInfo1 := cs.setPodGroupInfo(podInfo1.Pod, podInfo1.InitialAttemptTimestamp)
	pgInfo2 := cs.setPodGroupInfo(podInfo2.Pod, podInfo2.InitialAttemptTimestamp)

	priority1 := pgInfo1.priority
	priority2 := pgInfo2.priority

	if priority1 != priority2 {
		return priority1 > priority2
	}

	time1 := pgInfo1.timestamp
	time2 := pgInfo2.timestamp

	if !time1.Equal(time2) {
		return time1.Before(time2)
	}

	key1 := pgInfo1.key
	key2 := pgInfo2.key
	return key1 < key2
}

// getPodGroupkey returns a key of a PodGroup in the form of namespace/podGroupName.
func getPodGroupKey(namespace, podGroupName string) string {
	return fmt.Sprintf("%v/%v", namespace, podGroupName)
}

// getPodGroupInfo returns the PodGroup that a pod belongs to.
func (cs *Coscheduling) getPodGroupInfo(p *v1.Pod) (*PodGroupInfo, bool) {
	podGroupName, _, _ := GetPodGroupLabels(p)
	key := getPodGroupKey(p.Namespace, podGroupName)
	pgInfo, exist := cs.podGroupInfos.Load(key)
	if !exist {
		return nil, false
	}
	return pgInfo.(*PodGroupInfo), true
}

// setPodGroupInfo creates or updates a PodGroup and returns it.
// (1) Create a new PodGroup if the PodGroup does not exist.
// (2) Update minAvailable and priority values of the existing PodGroup.
// (3) Return the new created or existing PodGroup.
func (cs *Coscheduling) setPodGroupInfo(p *v1.Pod, t time.Time) *PodGroupInfo {
	podGroupName, minAvailable, _ := GetPodGroupLabels(p)
	key := getPodGroupKey(p.Namespace, podGroupName)
	pgInfo, exist := cs.podGroupInfos.Load(key)
	if !exist {
		pgInfo = &PodGroupInfo{
			name:         podGroupName,
			key:          key,
			priority:     pod.GetPodPriority(p),
			timestamp:    t,
			minAvailable: minAvailable,
		}
		cs.podGroupInfos.Store(key, pgInfo)
	} else {
		pgInfo.(*PodGroupInfo).minAvailable = minAvailable
		pgInfo.(*PodGroupInfo).priority = pod.GetPodPriority(p)
	}
	return pgInfo.(*PodGroupInfo)
}

// PreFilter validates that if the total number of pods belonging to the same `PodGroup` is less than `minAvailable`.
// If so, the scheduling process will be interrupted directly to avoid the partial Pods and hold the system resources
// until a timeout. It will reduce the overall scheduling time for the whole group.
func (cs *Coscheduling) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) *framework.Status {
	pgInfo := cs.setPodGroupInfo(p, time.Now())

	// check if the values of minAvailable are same.
	_, podMinAvailable, _ := GetPodGroupLabels(p)
	minAvailable := pgInfo.minAvailable
	if podMinAvailable != minAvailable {
		klog.Warningf("Pod %v has a different minAvailable (%v) as the PodGroup %v (%v)", p, podMinAvailable, pgInfo.key, minAvailable)
	}
	// check if the priorities are same.
	priority := pgInfo.priority
	podPriority := pod.GetPodPriority(p)
	if podPriority != priority {
		klog.Warningf("Pod %v has a different priority (%v) as the PodGroup %v (%v)", p, podPriority, pgInfo.key, priority)
	}

	if minAvailable <= 1 {
		return framework.NewStatus(framework.Success, "")
	}
	podGroupName := pgInfo.name
	total := cs.calculateTotalPods(podGroupName, p.Namespace)
	if total < minAvailable {
		klog.V(3).Infof("The count of podGroup %v/%v/%v is less than minAvailable(%d) in PreFilter: %d",
			p.Namespace, podGroupName, p.Name, minAvailable, total)
		return framework.NewStatus(framework.Unschedulable, "less than minAvailable")
	}

	return framework.NewStatus(framework.Success, "")
}

// PreFilterExtensions returns nil.
func (cs *Coscheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// Permit is the functions invoked by the framework at "permit" extension point.
func (cs *Coscheduling) Permit(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	pgInfo, exist := cs.getPodGroupInfo(p)
	if !exist {
		return framework.NewStatus(framework.Error, "failed to find a PodGroup associated with the pod"), 0
	}
	minAvailable := pgInfo.minAvailable
	if minAvailable <= 1 {
		return framework.NewStatus(framework.Success, ""), 0
	}
	podGroupName := pgInfo.name
	namespace := p.Namespace
	// TODO get actually scheduled(bind successfully) account from the SharedLister
	running := cs.calculateRunningPods(podGroupName, namespace)
	waiting := cs.calculateWaitingPods(podGroupName, namespace)
	current := running + waiting + 1

	if current < minAvailable {
		klog.V(3).Infof("The count of podGroup %v/%v/%v is not up to minAvailable(%d) in Permit: running(%d), waiting(%d)",
			p.Namespace, podGroupName, p.Name, minAvailable, running, waiting)
		// TODO Change the timeout to a dynamic value depending on the size of the `PodGroup`
		return framework.NewStatus(framework.Wait, ""), 10 * PermitWaitingTime
	}

	klog.V(3).Infof("The count of podGroup %v/%v/%v is up to minAvailable(%d) in Permit: running(%d), waiting(%d)",
		p.Namespace, podGroupName, p.Name, minAvailable, running, waiting)
	cs.frameworkHandle.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Namespace == namespace && waitingPod.GetPod().Labels[PodGroupName] == podGroupName {
			klog.V(3).Infof("Permit allows the pod: %v/%v", podGroupName, waitingPod.GetPod().Name)
			waitingPod.Allow(cs.Name())
		}
	})

	return framework.NewStatus(framework.Success, ""), 0
}

// Unreserve rejects all other Pods in the PodGroup when one of the pods in the group times out.
func (cs *Coscheduling) Unreserve(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
	podGroupName, exist := p.Labels[PodGroupName]
	if !exist {
		return
	}

	cs.frameworkHandle.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Namespace == p.Namespace && waitingPod.GetPod().Labels[PodGroupName] == podGroupName {
			klog.V(3).Infof("Unreserve rejects the pod: %v/%v", podGroupName, waitingPod.GetPod().Name)
			waitingPod.Reject(cs.Name())
		}
	})
}

// GetPodGroupLabels will check the pod if belongs to a  PodGroup. If so, it will return the
// podGroupName、minAvailable of the PodGroup. If not, it will return pod name and 0.
func GetPodGroupLabels(p *v1.Pod) (string, int32, error) {
	podGroupName, exist := p.Labels[PodGroupName]
	if !exist || podGroupName == "" {
		return p.Name, 0, nil
	}
	minAvailable, exist := p.Labels[PodGroupMinAvailable]
	if !exist || minAvailable == "" {
		return podGroupName, 0, nil
	}
	minNum, err := strconv.Atoi(minAvailable)
	if err != nil {
		klog.Errorf("GetPodGroupLabels err in coscheduling %v/%v : %v", p.Namespace, p.Name, err.Error())
		return podGroupName, 0, err
	}
	return podGroupName, int32(minNum), nil
}

func (cs *Coscheduling) calculateTotalPods(podGroupName, namespace string) int32 {
	// TODO get the total pods from the scheduler cache and queue instead of the hack manner.
	selector := labels.Set{PodGroupName: podGroupName}.AsSelector()
	pods, err := cs.podLister.Pods(namespace).List(selector)
	if err != nil {
		klog.Error(err)
		return 0
	}
	return int32(len(pods))
}

func (cs *Coscheduling) calculateRunningPods(podGroupName, namespace string) int32 {
	pods, err := cs.frameworkHandle.SnapshotSharedLister().Pods().FilteredList(func(pod *v1.Pod) bool {
		if pod.Labels[PodGroupName] == podGroupName && pod.Namespace == namespace && pod.Status.Phase == v1.PodRunning {
			return true
		}
		return false
	}, labels.NewSelector())

	if err != nil {
		klog.Error(err)
		return 0
	}

	return int32(len(pods))
}

func (cs *Coscheduling) calculateWaitingPods(podGroupName, namespace string) int32 {
	waiting := 0
	// Calculate the waiting pods.
	// TODO keep a cache of PodGroup size.
	cs.frameworkHandle.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Labels[PodGroupName] == podGroupName && waitingPod.GetPod().Namespace == namespace {
			waiting++
		}
	})

	return int32(waiting)
}
