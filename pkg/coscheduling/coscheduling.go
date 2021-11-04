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
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// Coscheduling is a plugin that schedules pods in a group.
type Coscheduling struct {
	frameworkHandler framework.Handle
	pgMgr            core.Manager
	scheduleTimeout  *time.Duration
}

var _ framework.QueueSortPlugin = &Coscheduling{}
var _ framework.PreFilterPlugin = &Coscheduling{}
var _ framework.PostFilterPlugin = &Coscheduling{}
var _ framework.PermitPlugin = &Coscheduling{}
var _ framework.ReservePlugin = &Coscheduling{}
var _ framework.PostBindPlugin = &Coscheduling{}
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

	conf, err := clientcmd.BuildConfigFromFlags(args.KubeMaster, args.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to init rest.Config: %v", err)
	}
	pgClient := pgclientset.NewForConfigOrDie(conf)
	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()

	fieldSelector, err := fields.ParseSelector(",status.phase!=" + string(v1.PodSucceeded) + ",status.phase!=" + string(v1.PodFailed))
	if err != nil {
		klog.ErrorS(err, "ParseSelector failed")
		os.Exit(1)
	}
	informerFactory := informers.NewSharedInformerFactoryWithOptions(handle.ClientSet(), 0, informers.WithTweakListOptions(func(opt *metav1.ListOptions) {
		opt.LabelSelector = util.PodGroupLabel
		opt.FieldSelector = fieldSelector.String()
	}))
	podInformer := informerFactory.Core().V1().Pods()

	scheduleTimeDuration := time.Duration(args.PermitWaitingTimeSeconds) * time.Second
	deniedPGExpirationTime := time.Duration(args.DeniedPGExpirationTimeSeconds) * time.Second

	ctx := context.TODO()

	pgMgr := core.NewPodGroupManager(pgClient, handle.SnapshotSharedLister(), &scheduleTimeDuration, &deniedPGExpirationTime, pgInformer, podInformer)
	plugin := &Coscheduling{
		frameworkHandler: handle,
		pgMgr:            pgMgr,
		scheduleTimeout:  &scheduleTimeDuration,
	}
	pgInformerFactory.Start(ctx.Done())
	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pgInformer.Informer().HasSynced, podInformer.Informer().HasSynced) {
		err := fmt.Errorf("WaitForCacheSync failed")
		klog.ErrorS(err, "Cannot sync caches")
		return nil, err
	}
	return plugin, nil
}

func (cs *Coscheduling) EventsToRegister() []framework.ClusterEvent {
	return []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.Add},
		// TODO: once bump the dependency to k8s 1.22, addd custom object events.
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
	creationTime1 := cs.pgMgr.GetCreationTimestamp(podInfo1.Pod, podInfo1.InitialAttemptTimestamp)
	creationTime2 := cs.pgMgr.GetCreationTimestamp(podInfo2.Pod, podInfo2.InitialAttemptTimestamp)
	if creationTime1.Equal(creationTime2) {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}
	return creationTime1.Before(creationTime2)
}

// PreFilter performs the following validations.
// 1. Whether the PodGroup that the Pod belongs to is on the deny list.
// 2. Whether the total number of pods in a PodGroup is less than its `minMember`.
func (cs *Coscheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) *framework.Status {
	// If any validation failed, a no-op state data is injected to "state" so that in later
	// phases we can tell whether the failure comes from PreFilter or not.
	if err := cs.pgMgr.PreFilter(ctx, pod); err != nil {
		klog.ErrorS(err, "PreFilter failed", "pod", klog.KObj(pod))
		state.Write(cs.getStateKey(), NewNoopStateData())
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	return framework.NewStatus(framework.Success, "")
}

// PostFilter is used to rejecting a group of pods if a pod does not pass PreFilter or Filter.
func (cs *Coscheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod,
	filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	// Check if the failure comes from PreFilter or not.
	_, err := state.Read(cs.getStateKey())
	if err == nil {
		state.Delete(cs.getStateKey())
		return &framework.PostFilterResult{}, framework.NewStatus(framework.Unschedulable)
	}

	pgName, pg := cs.pgMgr.GetPodGroup(pod)
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
		if waitingPod.GetPod().Namespace == pod.Namespace && waitingPod.GetPod().Labels[util.PodGroupLabel] == pg.Name {
			klog.V(3).InfoS("PostFilter rejects the pod", "podGroup", klog.KObj(pg), "pod", klog.KObj(waitingPod.GetPod()))
			waitingPod.Reject(cs.Name(), "optimistic rejection in PostFilter")
		}
	})
	cs.pgMgr.AddDeniedPodGroup(pgName)
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
	fullName := util.GetPodGroupFullName(pod)
	if len(fullName) == 0 {
		return framework.NewStatus(framework.Success, ""), 0
	}
	waitTime := *cs.scheduleTimeout
	ready, err := cs.pgMgr.Permit(ctx, pod, nodeName)
	if err != nil {
		_, pg := cs.pgMgr.GetPodGroup(pod)
		if pg == nil {
			return framework.NewStatus(framework.Unschedulable, "PodGroup not found"), 0
		}
		if wait := util.GetWaitTimeDuration(pg, cs.scheduleTimeout); wait != 0 {
			waitTime = wait
		}
		if err == util.ErrorWaiting {
			klog.InfoS("Pod is waiting to be scheduled to node", "pod", klog.KObj(pod), "node", nodeName)
			return framework.NewStatus(framework.Wait, ""), waitTime
		}
		klog.ErrorS(err, "Permit error")
		return framework.NewStatus(framework.Unschedulable, err.Error()), 0
	}

	klog.V(5).InfoS("Pod requires pgName", "pod", klog.KObj(pod), "podGroup", fullName)
	if !ready {
		return framework.NewStatus(framework.Wait, ""), waitTime
	}

	cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if util.GetPodGroupFullName(waitingPod.GetPod()) == fullName {
			klog.V(3).InfoS("Permit allows", "pod", klog.KObj(waitingPod.GetPod()))
			waitingPod.Allow(cs.Name())
		}
	})
	klog.V(3).InfoS("Permit allows", "pod", klog.KObj(pod))
	return framework.NewStatus(framework.Success, ""), 0
}

// Reserve is the functions invoked by the framework at "reserve" extension point.
func (cs *Coscheduling) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	return nil
}

// Unreserve rejects all other Pods in the PodGroup when one of the pods in the group times out.
func (cs *Coscheduling) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	pgName, pg := cs.pgMgr.GetPodGroup(pod)
	if pg == nil {
		return
	}
	cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if waitingPod.GetPod().Namespace == pod.Namespace && waitingPod.GetPod().Labels[util.PodGroupLabel] == pg.Name {
			klog.V(3).InfoS("Unreserve rejects", "pod", klog.KObj(waitingPod.GetPod()), "podGroup", klog.KObj(pg))
			waitingPod.Reject(cs.Name(), "rejection in Unreserve")
		}
	})
	cs.pgMgr.AddDeniedPodGroup(pgName)
	cs.pgMgr.DeletePermittedPodGroup(pgName)
}

// PostBind is called after a pod is successfully bound. These plugins are used update PodGroup when pod is bound.
func (cs *Coscheduling) PostBind(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, nodeName string) {
	klog.V(5).InfoS("PostBind", "pod", klog.KObj(pod))
	cs.pgMgr.PostBind(ctx, pod, nodeName)
}

// rejectPod rejects pod in cache
func (cs *Coscheduling) rejectPod(uid types.UID) {
	waitingPod := cs.frameworkHandler.GetWaitingPod(uid)
	if waitingPod == nil {
		return
	}
	waitingPod.Reject(Name, "")
}

func (cs *Coscheduling) getStateKey() framework.StateKey {
	return framework.StateKey(fmt.Sprintf("Prefilter-%v", cs.Name()))
}

type noopStateData struct {
}

func NewNoopStateData() framework.StateData {
	return &noopStateData{}
}

func (d *noopStateData) Clone() framework.StateData {
	return d
}
