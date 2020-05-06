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
	// Key is the name of Namespace/PodGroup.
	podGroupInfos sync.Map
}

// PodGroupInfo is a wrapper to a PodGroup with additional information.
// TODO implement a timeout based gc for the PodGroupInfos map
type PodGroupInfo struct {
	name string
	// timestamp stores the timestamp of the initialization time of PodGroup.
	timestamp time.Time
}

var _ framework.QueueSortPlugin = &Coscheduling{}
var _ framework.PreFilterPlugin = &Coscheduling{}
var _ framework.PermitPlugin = &Coscheduling{}
var _ framework.UnreservePlugin = &Coscheduling{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name                 = "coscheduling"
	PodGroupName         = "pod-group.scheduling.sigs.k8s.io/name"
	PodGroupMinAvailable = "pod-group.scheduling.sigs.k8s.io/min-available"
	// TODO make this configurable
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
// 1. compare the priority of pod
// 2. compare the timestamp of the initialization time of PodGroup
// 3. compare the key of PodGroup
func (cs *Coscheduling) Less(podInfo1 *framework.PodInfo, podInfo2 *framework.PodInfo) bool {
	pod1 := podInfo1.Pod
	pod2 := podInfo2.Pod
	priority1 := pod.GetPodPriority(pod1)
	priority2 := pod.GetPodPriority(pod2)

	if priority1 != priority2 {
		return priority1 > priority2
	}

	pgInfo1 := cs.getPodGroupInfo(podInfo1)
	pgInfo2 := cs.getPodGroupInfo(podInfo2)
	time1 := pgInfo1.timestamp
	time2 := pgInfo2.timestamp

	if !time1.Equal(time2) {
		return time1.Before(time2)
	}

	key1 := fmt.Sprintf("%v/%v", podInfo1.Pod.Namespace, pgInfo1.name)
	key2 := fmt.Sprintf("%v/%v", podInfo2.Pod.Namespace, pgInfo2.name)
	return key1 < key2
}

func (cs *Coscheduling) getPodGroupInfo(p *framework.PodInfo) *PodGroupInfo {
	podGroupName, min, err := GetPodGroupLabels(p.Pod)
	if err == nil && podGroupName != "" && min > 1 {
		key := fmt.Sprintf("%v/%v", p.Pod.Namespace, podGroupName)
		pgInfo, ok := cs.podGroupInfos.Load(key)
		if !ok {
			pgInfo = &PodGroupInfo{
				name:      podGroupName,
				timestamp: p.InitialAttemptTimestamp,
			}
			cs.podGroupInfos.Store(key, pgInfo)
		}
		return pgInfo.(*PodGroupInfo)
	}

	// If the pod is regular pod, return object of PodGroupInfo but not store in PodGroupInfos.
	// The purpose is to facilitate unified comparison.
	return &PodGroupInfo{name: "", timestamp: p.InitialAttemptTimestamp}
}

// PreFilter validates that if the total number of pods belonging to the same `PodGroup` is less than `minAvailable`.
// If so, the scheduling process will be interrupted directly to avoid the partial Pods holding system resources
// until a timeout. It will reduce the overall scheduling time for the whole group
func (cs *Coscheduling) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) *framework.Status {
	podGroupName, minAvailable, err := GetPodGroupLabels(p)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error())
	}
	if podGroupName == "" || minAvailable <= 1 {
		return framework.NewStatus(framework.Success, "")
	}

	total := cs.calculateTotalPods(podGroupName, p.Namespace)
	if total < minAvailable {
		klog.V(3).Infof("The count of podGroup %v/%v/%v is not up to minAvailable(%d) in PreFilter: %d",
			p.Namespace, podGroupName, p.Name, minAvailable, total)
		return framework.NewStatus(framework.Unschedulable, "less than minAvailable")
	}

	return framework.NewStatus(framework.Success, "")
}

func (cs *Coscheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// Permit is the functions invoked by the framework at "permit" extension point.
func (cs *Coscheduling) Permit(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	podGroupName, minAvailable, err := GetPodGroupLabels(p)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error()), 0
	}
	if podGroupName == "" || minAvailable <= 1 {
		return framework.NewStatus(framework.Success, ""), 0
	}

	namespace := p.Namespace
	// TODO get actually scheduled(bind successfully) account from the SharedLister
	running := cs.calculateRunningPods(podGroupName, namespace)
	waiting := cs.calculateWaitingPods(podGroupName, namespace)
	current := running + waiting + 1

	if current < minAvailable {
		klog.V(3).Infof("The count of podGroup %v/%v/%v is not up to minAvailable(%d) in Permit: running(%d), waiting(%d)",
			p.Namespace, podGroupName, p.Name, minAvailable, running, waiting)
		// TODO Change the timeout to dynamic value depends on the size of the `PodGroup`
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

// GetPodGroupLabels will check the pod if belongs to some podGroup. If so, it will return the
// podGroupName、minAvailable of podGroup. If not, it will return "" as podGroupName.
func GetPodGroupLabels(p *v1.Pod) (string, int, error) {
	podGroupName, exist := p.Labels[PodGroupName]
	if !exist || podGroupName == "" {
		return "", 0, nil
	}
	minAvailable, exist := p.Labels[PodGroupMinAvailable]
	if !exist || minAvailable == "" {
		return "", 0, nil
	}
	minNum, err := strconv.Atoi(minAvailable)
	if err != nil {
		klog.Errorf("GetPodGroupLabels err in coschduling %v/%v : %v", p.Namespace, p.Name, err.Error())
		return "", 0, err
	}
	return podGroupName, minNum, nil
}

func (cs *Coscheduling) calculateTotalPods(podGroupName, namespace string) int {
	// TODO get the total pods from the scheduler cache and queue instead of the hack manner
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
	// TODO keep a cache of podgroup size.
	cs.frameworkHandle.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Labels[PodGroupName] == podGroupName && waitingPod.GetPod().Namespace == namespace {
			waiting++
		}
	})

	return waiting
}
