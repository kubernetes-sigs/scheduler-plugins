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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// Coscheduling is a plugin that schedules pods in a group.
type Coscheduling struct {
	frameworkHandler framework.FrameworkHandle
	pgMgr            core.Manager
	scheduleTimeout  *time.Duration
}

var _ framework.QueueSortPlugin = &Coscheduling{}
var _ framework.PreFilterPlugin = &Coscheduling{}
var _ framework.PermitPlugin = &Coscheduling{}
var _ framework.ReservePlugin = &Coscheduling{}
var _ framework.PostBindPlugin = &Coscheduling{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = "Coscheduling"
)

// New initializes a new plugin and returns it.
func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
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
		klog.Fatalf("ParseSelector failed %+v", err)
	}
	informerFactory := informers.NewSharedInformerFactoryWithOptions(handle.ClientSet(), 0, informers.WithTweakListOptions(func(opt *metav1.ListOptions) {
		opt.LabelSelector = util.PodGroupLabel
		opt.FieldSelector = fieldSelector.String()
	}))
	podInformer := informerFactory.Core().V1().Pods()

	var (
		scheduleTimeDuration = util.DefaultWaitTime
	)
	if args.PermitWaitingTimeSeconds != 0 {
		scheduleTimeDuration = time.Duration(args.PermitWaitingTimeSeconds) * time.Second
	}
	ctx := context.TODO()

	pgMgr := core.NewPodGroupManager(pgClient, handle.SnapshotSharedLister(), &scheduleTimeDuration, pgInformer, podInformer)
	plugin := &Coscheduling{
		frameworkHandler: handle,
		pgMgr:            pgMgr,
		scheduleTimeout:  &scheduleTimeDuration,
	}
	pgInformerFactory.Start(ctx.Done())
	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pgInformer.Informer().HasSynced, podInformer.Informer().HasSynced) {
		klog.Error("Cannot sync caches")
		return nil, fmt.Errorf("WaitForCacheSync failed")
	}
	return plugin, nil
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
	prio1 := podutil.GetPodPriority(podInfo1.Pod)
	prio2 := podutil.GetPodPriority(podInfo2.Pod)
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
	if err := cs.pgMgr.PreFilter(ctx, pod); err != nil {
		klog.Error(err)
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	return framework.NewStatus(framework.Success, "")
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
			return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PodGroup not found"), waitTime
		}
		if wait := util.GetWaitTimeDuration(pg, cs.scheduleTimeout); wait != 0 {
			waitTime = wait
		}
		if err == util.ErrorWaiting {
			klog.Infof("Pod: %v is waiting to be scheduled to node: %v", core.GetNamespacedName(pod), nodeName)
			return framework.NewStatus(framework.Wait, ""), waitTime
		}
		klog.Infof("Permit error %v", err)
		return framework.NewStatus(framework.Unschedulable, err.Error()), waitTime
	}

	klog.V(5).Infof("Pod requires pgName %v", fullName)
	if !ready {
		return framework.NewStatus(framework.Wait, ""), waitTime
	}

	cs.frameworkHandler.IterateOverWaitingPods(func(waitingPod framework.WaitingPod) {
		if util.GetPodGroupFullName(waitingPod.GetPod()) == fullName {
			klog.V(3).Infof("Permit allows the pod: %v", core.GetNamespacedName(waitingPod.GetPod()))
			waitingPod.Allow(cs.Name())
		}
	})
	klog.V(3).Infof("Permit allows the pod: %v", core.GetNamespacedName(pod))
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
			klog.V(3).Infof("Unreserve rejects the pod: %v/%v", pgName, waitingPod.GetPod().Name)
			waitingPod.Reject(cs.Name())
		}
	})
	cs.pgMgr.AddDeniedPodGroup(pgName)
}

// PostBind is called after a pod is successfully bound. These plugins are used update PodGroup when pod is bound.
func (cs *Coscheduling) PostBind(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, nodeName string) {
	klog.V(5).Infof("PostBind pod: %v", core.GetNamespacedName(pod))
	cs.pgMgr.PostBind(ctx, pod, nodeName)
}

// rejectPod rejects pod in cache
func (cs *Coscheduling) rejectPod(uid types.UID) {
	waitingPod := cs.frameworkHandler.GetWaitingPod(uid)
	if waitingPod == nil {
		return
	}
	waitingPod.Reject(Name)
}
