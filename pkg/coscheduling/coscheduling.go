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
	api "k8s.io/kubernetes/pkg/api/v1/pod"
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
	minAvailable int
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
	pgInfo1 := cs.getPodGroupInfo(podInfo1)
	pgInfo2 := cs.getPodGroupInfo(podInfo2)

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

	return pgInfo1.key < pgInfo2.key
}

// getPodGroupkey returns a key of a PodGroup in the form of namespace/PodGroupName.
func getPodGroupKey(namespace, podGroupName string) string {
	return fmt.Sprintf("%v/%v", namespace, podGroupName)
}

// getPodGroupInfo creates a PodGroup if not exsiting;
// and store it in a local cache map if the PodGroup has more than one Pod.
func (cs *Coscheduling) getPodGroupInfo(podInfo *framework.PodInfo) *PodGroupInfo {
	pod := podInfo.Pod
	podGroupName, minAvailable, _ := GetPodGroupLabels(pod)
	key := getPodGroupKey(pod.Namespace, podGroupName)

	// If it is a PodGroup with more than one pod, check if there is a PodGroup in PodGroupInfos
	if len(podGroupName) > 0 && minAvailable >= 1 {
		pgInfo, exist := cs.podGroupInfos.Load(key)
		if exist {
			return pgInfo.(*PodGroupInfo)
		}
	}

	// If it's a non-existing PodGroup with more than 1 pod or a regular pod,
	// create a PodGroup
	pgInfo := &PodGroupInfo{
		name:         podGroupName,
		key:          key,
		priority:     api.GetPodPriority(pod),
		timestamp:    podInfo.InitialAttemptTimestamp,
		minAvailable: minAvailable,
	}

	// If it is a PodGroup, store it in PodGroupInfos
	if len(podGroupName) > 0 && minAvailable >= 1 {
		cs.podGroupInfos.Store(key, pgInfo)
	}
	return pgInfo
}

// PreFilter validates that if the total number of pods belonging to the same `PodGroup` is less than `minAvailable`.
// If so, the scheduling process will be interrupted directly to avoid the partial Pods and hold the system resources
// until a timeout. It will reduce the overall scheduling time for the whole group.
// It also validates if minAvailables and priorities of all the pods in a PodGroup are the same.
func (cs *Coscheduling) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) *framework.Status {
	var pgInfo interface{}
	var exist bool

	podGroupName, minAvailable, err := GetPodGroupLabels(p)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error())
	}

	if len(podGroupName) == 0 {
		return framework.NewStatus(framework.Success, "")
	}

	pgKey := getPodGroupKey(p.Namespace, podGroupName)
	if pgInfo, exist = cs.podGroupInfos.Load(pgKey); !exist {
		klog.Errorf("Failed to find PodGroup %v for Pod %v", pgKey, p.Name)
		return framework.NewStatus(framework.Unschedulable, "No PodGroup found")
	}
	pgMinAvailable := pgInfo.(*PodGroupInfo).minAvailable
	// check if the values of minAvailable are same.
	if minAvailable != pgMinAvailable {
		klog.Errorf("Pod %v has a different minAvailable (%v) as the PodGroup %v (%v)", p.Name, minAvailable, pgKey, pgMinAvailable)
		return framework.NewStatus(framework.Unschedulable, "PodGroupMinAvailables do not match")
	}
	// check if the priorities are same.
	pgPriority := pgInfo.(*PodGroupInfo).priority
	priority := pod.GetPodPriority(p)
	if pgPriority != priority {
		klog.Errorf("Pod %v has a different priority (%v) as the PodGroup %v (%v)", p.Name, priority, pgKey, pgPriority)
		return framework.NewStatus(framework.Unschedulable, "Priorities do not match")
	}

	total := cs.calculateTotalPods(podGroupName, p.Namespace)
	if total < minAvailable {
		klog.V(3).Infof("The count of PodGroup %v/%v/%v is less than minAvailable(%d) in PreFilter: %d",
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
	podGroupName, minAvailable, err := GetPodGroupLabels(p)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error()), 0
	}
	if len(podGroupName) == 0 || minAvailable <= 1 {
		return framework.NewStatus(framework.Success, ""), 0
	}
	namespace := p.Namespace
	// TODO get actually scheduled(bind successfully) account from the SharedLister
	running := cs.calculateRunningPods(podGroupName, namespace)
	waiting := cs.calculateWaitingPods(podGroupName, namespace)
	current := running + waiting + 1

	if current < minAvailable {
		klog.V(3).Infof("The count of PodGroup %v/%v/%v is not up to minAvailable(%d) in Permit: running(%d), waiting(%d)",
			p.Namespace, podGroupName, p.Name, minAvailable, running, waiting)
		// TODO Change the timeout to a dynamic value depending on the size of the `PodGroup`
		return framework.NewStatus(framework.Wait, ""), 10 * PermitWaitingTime
	}

	klog.V(3).Infof("The count of PodGroup %v/%v/%v is up to minAvailable(%d) in Permit: running(%d), waiting(%d)",
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
// podGroupName、minAvailable of the PodGroup. If not, it will return the pod name and 0.
func GetPodGroupLabels(p *v1.Pod) (string, int, error) {
	podGroupName, exist := p.Labels[PodGroupName]
	if !exist || len(podGroupName) == 0 {
		return "", 0, nil
	}
	minAvailable, exist := p.Labels[PodGroupMinAvailable]
	if !exist || len(minAvailable) == 0 {
		return "", 0, nil
	}
	minNum, err := strconv.Atoi(minAvailable)
	if err != nil {
		klog.Errorf("GetPodGroupLabels err in coscheduling %v/%v : %v", p.Namespace, p.Name, err.Error())
		return "", 0, err
	}
	if minNum < 1 {
		minNum = 1
	}
	return podGroupName, minNum, nil
}

func (cs *Coscheduling) calculateTotalPods(podGroupName, namespace string) int {
	// TODO get the total pods from the scheduler cache and queue instead of the hack manner.
	selector := labels.Set{PodGroupName: podGroupName}.AsSelector()
	pods, err := cs.podLister.Pods(namespace).List(selector)
	if err != nil {
		klog.Error(err)
		return 0
	}
	return len(pods)
}

func (cs *Coscheduling) calculateRunningPods(podGroupName, namespace string) int {
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

	return len(pods)
}

func (cs *Coscheduling) calculateWaitingPods(podGroupName, namespace string) int {
	waiting := 0
	// Calculate the waiting pods.
	// TODO keep a cache of PodGroup size.
	cs.frameworkHandle.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Labels[PodGroupName] == podGroupName && waitingPod.GetPod().Namespace == namespace {
			waiting++
		}
	})

	return waiting
}
