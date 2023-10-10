package rtpreemptive

import (
	"context"
	"errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/utils/clock"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/laxity"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/preemption"
)

const (
	// NameLLF is the name of the plugin used in the plugin registry and configuration
	NameLLF = "LLFPreemptiveScheduling"
)

var (
	// sort pods based on laxity
	_ framework.QueueSortPlugin = &LLFPreemptiveScheduling{}
	_ framework.PreFilterPlugin = &LLFPreemptiveScheduling{}
	_ framework.FilterPlugin    = &LLFPreemptiveScheduling{}
	// _ framework.PostFilterPlugin = &LLFPreemptiveScheduling{}
)

// LLFPreemptiveScheduling implements several plugins to perform soft real-time
// least laxity first scheduling
type LLFPreemptiveScheduling struct {
	fh                framework.Handle
	laxityManager     laxity.Manager
	preemptionManager preemption.Manager
	clock             clock.Clock
}

// Name returns name of the plugin, It is used in logs, etc.
func (rp *LLFPreemptiveScheduling) Name() string {
	return NameLLF
}

// NewEDF initializes a new LLFPreemptiveScheduling plugin and return it.
func NewLLF(_ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	podLister := fh.SharedInformerFactory().Core().V1().Pods().Lister()
	nodeLister := fh.SharedInformerFactory().Core().V1().Nodes().Lister()
	nodeInfoLister := fh.SnapshotSharedLister().NodeInfos()
	laxityManager := laxity.NewLaxityManager()
	prioFunc := func(p *v1.Pod) int64 {
		laxity, _ := laxityManager.GetPodLaxity(p)
		return -int64(laxity)
	}
	llfSched := &LLFPreemptiveScheduling{
		fh:                fh,
		laxityManager:     laxityManager,
		preemptionManager: preemption.NewPreemptionManager(podLister, nodeLister, nodeInfoLister, fh.ClientSet(), prioFunc),
		clock:             clock.RealClock{},
	}
	fh.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    llfSched.handlePodAdd,
		UpdateFunc: llfSched.handlePodUpdate,
		DeleteFunc: llfSched.handlePodDelete,
	})
	return llfSched, nil
}

func (rp *LLFPreemptiveScheduling) handlePodAdd(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert obj to *v1.Pod", "obj", obj)
		return
	}
	if pod.Status.Phase == v1.PodRunning {
		klog.InfoS("Pod is already running, starting pod execution", "pod", klog.KObj(pod))
		rp.laxityManager.StartPodExecution(pod)
	}
}

func (rp *LLFPreemptiveScheduling) handlePodUpdate(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert oldObj to *v1.Pod", "oldObj", oldObj)
		return
	}
	newPod, ok := newObj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert newObj to *v1.Pod", "newObj", newObj)
		return
	}
	if oldPod.Status.Phase == v1.PodRunning && newPod.Status.Phase == v1.PodPaused {
		klog.InfoS("Pod was running but now paused, pausing pod execution", "pod", klog.KObj(oldPod))
		rp.laxityManager.PausePodExecution(newPod)
		return
	}
	if oldPod.Status.Phase == v1.PodPaused && newPod.Status.Phase == v1.PodRunning {
		klog.InfoS("Pod was paused but now resumed, starting pod execution again", "pod", klog.KObj(oldPod))
		rp.laxityManager.StartPodExecution(newPod)
		return
	}
	if newPod.Status.Phase == v1.PodSucceeded || newPod.Status.Phase == v1.PodFailed {
		klog.InfoS("Pod has exited", "pod", klog.KObj(oldPod), "exitStatus", newPod.Status.Phase)
		rp.laxityManager.RemovePodExecution(newPod)
		return
	}
}

func (rp *LLFPreemptiveScheduling) handlePodDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert obj to *v1.Pod", "obj", obj)
		return
	}
	rp.laxityManager.RemovePodExecution(pod)
}

// QueueSort Plugin
// Less is used to sort pods in the scheduling queue in the following order.
//  1. Compare the priorities of Pods.
//  2. Compare the laxity of jobs
//  3. Compare the keys of Pods: <namespace>/<podname>.
func (rp *LLFPreemptiveScheduling) Less(podInfo1, podInfo2 *framework.QueuedPodInfo) bool {
	prio1 := corev1helpers.PodPriority(podInfo1.Pod)
	prio2 := corev1helpers.PodPriority(podInfo2.Pod)
	if prio1 != prio2 {
		return prio1 > prio2
	}
	l1, _ := rp.laxityManager.GetPodLaxity(podInfo1.Pod)
	l2, _ := rp.laxityManager.GetPodLaxity(podInfo2.Pod)
	if l1 == l2 {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}
	return l1 < l2
}

func (rp *LLFPreemptiveScheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	paused := pod.Status.Phase == v1.PodPaused
	markedPaused := rp.preemptionManager.IsPodMarkedPaused(pod)

	if markedPaused && !paused {
		return nil, framework.NewStatus(framework.Skip, "skipped as pod cannot be resumed")
	}
	if !markedPaused && paused {
		// resumed might be in progress but pod added back to scheduling queue
		// should not happen, likely an unexpected error
		err := errors.New("pod is not marked to be paused but currently at paused state")
		klog.ErrorS(err, "unexpected error")
		return nil, framework.AsStatus(err)
	}
	if markedPaused && paused {
		klog.InfoS("pod is paused and attempt to resume it", "pod", klog.KObj(pod))
		candidate := rp.preemptionManager.GetPausedCandidateOnNode(ctx, pod.Spec.NodeName)
		if candidate == nil {
			candidate = &preemption.Candidate{NodeName: pod.Spec.NodeName, Pod: pod}
		}
		if candidate.Pod.UID != pod.UID {
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "rejected as another paused pod has higher priority")
		}
		c, err := rp.preemptionManager.ResumeCandidate(ctx, candidate)
		if err == preemption.ErrPodNotPaused {
			klog.V(4).InfoS("pod was marked to be paused but is not paused", "pod", klog.KObj(pod))
			return nil, framework.NewStatus(framework.Skip)
		}
		if err != nil {
			klog.ErrorS(err, "failed to resume pod", "pod", klog.KObj(pod))
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable)
		}
		klog.V(4).InfoS("successfully resumed pod", "pod", klog.KObj(c.Pod))
		return nil, framework.NewStatus(framework.Skip, "skipped because pod is resumed successfully")
	}
	return nil, nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (rp *LLFPreemptiveScheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func (rp *LLFPreemptiveScheduling) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if len(pod.Spec.NodeName) > 0 && pod.Spec.NodeName != nodeInfo.Node().Name {
		// pod is already assigned to a node and it's not the same as given node
		// this happens when a paused pod re-enters the scheduling queue
		return framework.NewStatus(framework.UnschedulableAndUnresolvable)
	}

	if candidate := rp.preemptionManager.GetPausedCandidateOnNode(ctx, nodeInfo.Node().Name); candidate != nil {
		candidateLaxity, _ := rp.laxityManager.GetPodLaxity(candidate.Pod)
		podLaxity, _ := rp.laxityManager.GetPodLaxity(pod)
		if candidateLaxity < podLaxity {
			msg := "found a paused pod on node that need to be resumed"
			klog.V(4).InfoS(msg, "pausedPod", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
			c, err := rp.preemptionManager.ResumeCandidate(ctx, candidate)
			if err == nil {
				klog.V(4).InfoS("resumed candidate successfully", "candidate", klog.KObj(c.Pod), "node", c.NodeName)
				return framework.NewStatus(framework.UnschedulableAndUnresolvable, msg)
			}
			klog.V(4).InfoS("failed to resume paused pod, continue to schedule pod", "error", err, "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
		}
	}

	var unpausedPods []*v1.Pod
	for _, p := range nodeInfo.Pods {
		if p.Pod.Status.Phase != v1.PodPaused {
			unpausedPods = append(unpausedPods, p.Pod)
		}
	}
	nodeExcludePausedPods := framework.NewNodeInfo(unpausedPods...)
	nodeExcludePausedPods.SetNode(nodeInfo.Node())
	insufficientResources := noderesources.Fits(pod, nodeExcludePausedPods)
	if len(insufficientResources) != 0 {
		// We will keep all failure reasons.
		failureReasons := make([]string, 0, len(insufficientResources))
		for i := range insufficientResources {
			failureReasons = append(failureReasons, insufficientResources[i].Reason)
		}
		return framework.NewStatus(framework.Unschedulable, failureReasons...)
	}
	return nil
}
