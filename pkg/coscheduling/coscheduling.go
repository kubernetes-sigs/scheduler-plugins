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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// Coscheduling is a plugin that schedules pods in a group.
type Coscheduling struct {
	frameworkHandler framework.Handle
	pgMgr            core.Manager
	scheduleTimeout  *time.Duration
	pgBackoff        *time.Duration
}

var _ framework.QueueSortPlugin = &Coscheduling{}
var _ framework.PreFilterPlugin = &Coscheduling{}
var _ framework.PostFilterPlugin = &Coscheduling{}
var _ framework.PermitPlugin = &Coscheduling{}
var _ framework.ReservePlugin = &Coscheduling{}

var _ framework.EnqueueExtensions = &Coscheduling{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = "Coscheduling"
)

// New initializes and returns a new Coscheduling plugin.
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	args, ok := obj.(*config.CoschedulingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type CoschedulingArgs, got %T", obj)
	}

	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	client, err := client.New(handle.KubeConfig(), client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	// Performance improvement when retrieving list of objects by namespace or we'll log 'index not exist' warning.
	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddIndexers(cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	scheduleTimeDuration := time.Duration(args.PermitWaitingTimeSeconds) * time.Second
	pgMgr := core.NewPodGroupManager(
		client,
		handle.SnapshotSharedLister(),
		&scheduleTimeDuration,
		// Keep the podInformer (from frameworkHandle) as the single source of Pods.
		handle.SharedInformerFactory().Core().V1().Pods(),
	)
	plugin := &Coscheduling{
		frameworkHandler: handle,
		pgMgr:            pgMgr,
		scheduleTimeout:  &scheduleTimeDuration,
	}
	if args.PodGroupBackoffSeconds < 0 {
		err := fmt.Errorf("parse arguments failed")
		klog.ErrorS(err, "PodGroupBackoffSeconds cannot be negative")
		return nil, err
	} else if args.PodGroupBackoffSeconds > 0 {
		pgBackoff := time.Duration(args.PodGroupBackoffSeconds) * time.Second
		plugin.pgBackoff = &pgBackoff
	}
	return plugin, nil
}

func (cs *Coscheduling) EventsToRegister() []framework.ClusterEvent {
	// To register a custom event, follow the naming convention at:
	// https://git.k8s.io/kubernetes/pkg/scheduler/eventhandlers.go#L403-L410
	pgGVK := fmt.Sprintf("podgroups.v1alpha1.%v", scheduling.GroupName)
	return []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.Add},
		{Resource: framework.GVK(pgGVK), ActionType: framework.Add | framework.Update},
	}
}

// Name returns name of the plugin. It is used in logs, etc.
func (cs *Coscheduling) Name() string {
	return Name
}

// Less is used to sort pods in the scheduling queue in the following order.
// 1. Compare the priorities of Pods.
// 2. Compare the initialization timestamps of PodGroups or Pods.
// 3. Compare the keys of PodGroups/Pods: <namespace>/<podname>.
func (cs *Coscheduling) Less(podInfo1, podInfo2 *framework.QueuedPodInfo) bool {
	prio1 := corev1helpers.PodPriority(podInfo1.Pod)
	prio2 := corev1helpers.PodPriority(podInfo2.Pod)
	if prio1 != prio2 {
		return prio1 > prio2
	}
	creationTime1 := cs.pgMgr.GetCreationTimestamp(podInfo1.Pod, *podInfo1.InitialAttemptTimestamp)
	creationTime2 := cs.pgMgr.GetCreationTimestamp(podInfo2.Pod, *podInfo2.InitialAttemptTimestamp)
	if creationTime1.Equal(creationTime2) {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}
	return creationTime1.Before(creationTime2)
}

// PreFilter performs the following validations.
// 1. Whether the PodGroup that the Pod belongs to is on the deny list.
// 2. Whether the total number of pods in a PodGroup is less than its `minMember`.
func (cs *Coscheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// If PreFilter fails, return framework.UnschedulableAndUnresolvable to avoid
	// any preemption attempts.
	if err := cs.pgMgr.PreFilter(ctx, pod); err != nil {
		klog.ErrorS(err, "PreFilter failed", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}
	return nil, framework.NewStatus(framework.Success, "")
}

// PostFilter is used to reject a group of pods if a pod does not pass PreFilter or Filter.
func (cs *Coscheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod,
	filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	pgName, pg := cs.pgMgr.GetPodGroup(ctx, pod)
	if pg == nil {
		klog.V(4).InfoS("Pod does not belong to any group", "pod", klog.KObj(pod))
		return &framework.PostFilterResult{}, framework.NewStatus(framework.Unschedulable, "can not find pod group")
	}

	// This indicates there are already enough Pods satisfying the PodGroup,
	// so don't bother to reject the whole PodGroup.
	assigned := cs.pgMgr.CalculateAssignedPods(pg.Name, pod.Namespace)
	if assigned >= int(pg.Spec.MinMember) {
		klog.V(4).InfoS("Assigned pods", "podGroup", klog.KObj(pg), "assigned", assigned)
		return &framework.PostFilterResult{}, framework.NewStatus(framework.Unschedulable)
	}

	// If the gap is less than/equal 10%, we may want to try subsequent Pods
	// to see they can satisfy the PodGroup
	notAssignedPercentage := float32(int(pg.Spec.MinMember)-assigned) / float32(pg.Spec.MinMember)
	if notAssignedPercentage <= 0.1 {
		klog.V(4).InfoS("A small gap of pods to reach the quorum", "podGroup", klog.KObj(pg), "percentage", notAssignedPercentage)
		return &framework.PostFilterResult{}, framework.NewStatus(framework.Unschedulable)
	}

	// It's based on an implicit assumption: if the nth Pod failed,
	// it's inferrable other Pods belonging to the same PodGroup would be very likely to fail.
	cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Namespace == pod.Namespace && util.GetPodGroupLabel(waitingPod.GetPod()) == pg.Name {
			klog.V(3).InfoS("PostFilter rejects the pod", "podGroup", klog.KObj(pg), "pod", klog.KObj(waitingPod.GetPod()))
			waitingPod.Reject(cs.Name(), "optimistic rejection in PostFilter")
		}
	})

	if cs.pgBackoff != nil {
		pods, err := cs.frameworkHandler.SharedInformerFactory().Core().V1().Pods().Lister().Pods(pod.Namespace).List(
			labels.SelectorFromSet(labels.Set{v1alpha1.PodGroupLabel: util.GetPodGroupLabel(pod)}),
		)
		if err == nil && len(pods) >= int(pg.Spec.MinMember) {
			cs.pgMgr.BackoffPodGroup(pgName, *cs.pgBackoff)
		}
	}

	cs.pgMgr.DeletePermittedPodGroup(pgName)
	return &framework.PostFilterResult{}, framework.NewStatus(framework.Unschedulable,
		fmt.Sprintf("PodGroup %v gets rejected due to Pod %v is unschedulable even after PostFilter", pgName, pod.Name))
}

// PreFilterExtensions returns a PreFilterExtensions interface if the plugin implements one.
func (cs *Coscheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// Permit is the functions invoked by the framework at "Permit" extension point.
func (cs *Coscheduling) Permit(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (*framework.Status, time.Duration) {
	waitTime := *cs.scheduleTimeout
	s := cs.pgMgr.Permit(ctx, pod)
	var retStatus *framework.Status
	switch s {
	case core.PodGroupNotSpecified:
		return framework.NewStatus(framework.Success, ""), 0
	case core.PodGroupNotFound:
		return framework.NewStatus(framework.Unschedulable, "PodGroup not found"), 0
	case core.Wait:
		klog.InfoS("Pod is waiting to be scheduled to node", "pod", klog.KObj(pod), "nodeName", nodeName)
		_, pg := cs.pgMgr.GetPodGroup(ctx, pod)
		if wait := util.GetWaitTimeDuration(pg, cs.scheduleTimeout); wait != 0 {
			waitTime = wait
		}
		retStatus = framework.NewStatus(framework.Wait)
		// We will also request to move the sibling pods back to activeQ.
		cs.pgMgr.ActivateSiblings(pod, state)
	case core.Success:
		pgFullName := util.GetPodGroupFullName(pod)
		cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
			if util.GetPodGroupFullName(waitingPod.GetPod()) == pgFullName {
				klog.V(3).InfoS("Permit allows", "pod", klog.KObj(waitingPod.GetPod()))
				waitingPod.Allow(cs.Name())
			}
		})
		klog.V(3).InfoS("Permit allows", "pod", klog.KObj(pod))
		retStatus = framework.NewStatus(framework.Success)
		waitTime = 0
	}

	return retStatus, waitTime
}

// Reserve is the functions invoked by the framework at "reserve" extension point.
func (cs *Coscheduling) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	return nil
}

// Unreserve rejects all other Pods in the PodGroup when one of the pods in the group times out.
func (cs *Coscheduling) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	pgName, pg := cs.pgMgr.GetPodGroup(ctx, pod)
	if pg == nil {
		return
	}
	cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Namespace == pod.Namespace && util.GetPodGroupLabel(waitingPod.GetPod()) == pg.Name {
			klog.V(3).InfoS("Unreserve rejects", "pod", klog.KObj(waitingPod.GetPod()), "podGroup", klog.KObj(pg))
			waitingPod.Reject(cs.Name(), "rejection in Unreserve")
		}
	})
	cs.pgMgr.DeletePermittedPodGroup(pgName)
}
