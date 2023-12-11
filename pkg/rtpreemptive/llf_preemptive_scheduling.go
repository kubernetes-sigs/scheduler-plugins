package rtpreemptive

import (
	"context"
	"errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/utils/clock"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/laxity"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/preemption"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/priorityqueue"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/util"
)

const (
	// NameLLF is the name of the plugin used in the plugin registry and configuration
	NameLLF = "LLFPreemptiveScheduling"
)

var (
	_ framework.QueueSortPlugin  = &LLFPreemptiveScheduling{}
	_ framework.PreFilterPlugin  = &LLFPreemptiveScheduling{}
	_ framework.FilterPlugin     = &LLFPreemptiveScheduling{}
	_ framework.PostFilterPlugin = &LLFPreemptiveScheduling{}
)

// LLFPreemptiveScheduling implements several plugins to perform soft real-time
// least laxity first scheduling
type LLFPreemptiveScheduling struct {
	fh                framework.Handle
	podLister         corelisters.PodLister
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
		podLister:         podLister,
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
	oldPhase := oldPod.Status.Phase
	newPhase := newPod.Status.Phase
	switch {
	case oldPhase == v1.PodPending && newPhase == v1.PodRunning:
		rp.laxityManager.StartPodExecution(newPod)
	case oldPhase == v1.PodRunning && newPhase == v1.PodPaused:
		rp.laxityManager.PausePodExecution(newPod)
	case oldPhase == v1.PodPaused && newPhase == v1.PodRunning:
		rp.laxityManager.StartPodExecution(newPod)
	case (oldPhase == v1.PodRunning || oldPhase == v1.PodPaused || oldPhase == v1.PodPending) && (newPhase == v1.PodSucceeded || newPhase == v1.PodFailed):
		rp.laxityManager.RemovePodExecution(newPod)
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
	// TODO: remove debug log
	klog.InfoS("queuesort: pod1", "pod", klog.KObj(podInfo1.Pod), "laxity", l1)
	klog.InfoS("queuesort: pod2", "pod", klog.KObj(podInfo2.Pod), "laxity", l2)
	// ENDTODO

	if l1 == l2 {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}
	return l1 < l2
}

func (rp *LLFPreemptiveScheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	latestPod, _ := rp.podLister.Pods(pod.Namespace).Get(pod.Name)
	if latestPod != nil {
		pod = latestPod
	}
	paused := pod.Status.Phase == v1.PodPaused
	markedPaused := rp.preemptionManager.IsPodMarkedPaused(pod)
	klog.InfoS("debug: preFilter", "pod", klog.KObj(pod), "paused", paused, "markedPaused", markedPaused)
	if markedPaused && !paused {
		return nil, framework.NewStatus(framework.Skip, "skipped as pod cannot be resumed")
	}
	if markedPaused && paused {
		klog.V(4).InfoS("pod is paused and attempt to resume it", "pod", klog.KObj(pod))
		candidate := rp.preemptionManager.GetPausedCandidateOnNode(ctx, pod.Spec.NodeName)
		if candidate == nil {
			klog.V(4).InfoS("pod was marked to be paused but not found in preemption manager, attempt to resume", "pod", klog.KObj(pod))
			candidate = &preemption.Candidate{NodeName: pod.Spec.NodeName, Pod: pod}
		}
		if candidate.Pod.UID != pod.UID {
			klog.V(4).InfoS("another pod on the node has higher priority", "pod", klog.KObj(pod), "candidate", klog.KObj(candidate.Pod), "node", candidate.NodeName)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "rejected as another paused pod has higher priority")
		}
		if candidate.Preemptor != nil && candidate.Preemptor.Status.Phase == v1.PodPending {
			klog.InfoS("pod was preempted by a different pod that is still pending", "preemptor", klog.KObj(candidate.Preemptor), "pod", klog.KObj(pod))
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "rejected as the preemptor is not yet scheduled")
		}
		c, err := rp.preemptionManager.ResumeCandidate(ctx, candidate)
		if err != nil {
			klog.ErrorS(err, "failed to resume pod", "pod", klog.KObj(pod))
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable)
		}
		// TODO: change to level 4
		klog.InfoS("successfully resumed pod", "pod", klog.KObj(c.Pod))
		// ENDTODO
		return nil, framework.NewStatus(framework.Skip)
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
		if candidate.Preemptor != nil && candidate.Preemptor.Status.Phase == v1.PodPending && candidate.Preemptor.UID != pod.UID {
			klog.InfoS("not eligible to resume, pod was preempted by a different pod that is still pending", "candidate", klog.KObj(candidate.Pod), "preemptor", klog.KObj(candidate.Preemptor), "pod", klog.KObj(pod))
			goto skipResume
		}
		candidateLaxity, _ := rp.laxityManager.GetPodLaxity(candidate.Pod)
		podLaxity, _ := rp.laxityManager.GetPodLaxity(pod)
		// TODO: remove debug log
		klog.InfoS("filter: candidate", "pod", klog.KObj(candidate.Pod), "laxity", candidateLaxity, "phase", candidate.Pod.Status.Phase, "node", candidate.NodeName)
		klog.InfoS("filter: pod2sched", "pod", klog.KObj(pod), "laxity", podLaxity, "phase", pod.Status.Phase, "node", pod.Spec.NodeName)
		// ENDTODO
		if candidateLaxity < podLaxity {
			msg := "found a paused pod on node that need to be resumed"
			klog.V(4).InfoS(msg, "pausedPod", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
			c, err := rp.preemptionManager.ResumeCandidate(ctx, candidate)
			if err == nil {
				// TODO: change to level 4
				klog.InfoS("resumed candidate successfully", "candidate", klog.KObj(c.Pod), "node", c.NodeName)
				// ENDTODO
				return framework.NewStatus(framework.UnschedulableAndUnresolvable, msg)
			}
			klog.V(4).InfoS("failed to resume paused pod, continue to schedule pod", "error", err, "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
		}
	}
skipResume:
	var unpausedPods []*v1.Pod
	for _, p := range nodeInfo.Pods {
		if p.Pod.Status.Phase != v1.PodPaused {
			unpausedPods = append(unpausedPods, p.Pod)
			klog.InfoS("debug: filter adding unpaused pod", "pod", klog.KObj(p.Pod), "pod2Sched", klog.KObj(pod))
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

// PostFilter plugin is called when no node is found schedulable at Filter stage
// here it attempt to preempt a pod on available nodes
// if preemption is successful, return with nominated node and add pod to paused map
func (rp *LLFPreemptiveScheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	allNode, err := rp.fh.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "failed to list all nodes", "pod", klog.KObj(pod))
		return nil, framework.AsStatus(err)
	}
	var candidates []*preemption.Candidate
	for _, nodeInfo := range allNode {
		// skip node where preemption is not helpful
		if filteredNodeStatusMap[nodeInfo.Node().Name].Code() == framework.UnschedulableAndUnresolvable {
			klog.V(4).InfoS("skipping unschedulable and unresolvable node", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
			continue
		}
		candidate := rp.findCandidateOnNode(pod, nodeInfo)
		if candidate != nil {
			candidates = append(candidates, &preemption.Candidate{NodeName: nodeInfo.Node().Name, Pod: candidate})
		}
	}
	candidate := rp.selectCandidate(candidates)
	if candidate == nil {
		klog.ErrorS(errors.New("no preemptible candidates"), "select candidate failed", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "no preemptible candidates found")
	}
	// TODO: change to level 4
	klog.InfoS("found candidate pod to pause on node", "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
	// ENDTODO
	candidate.Preemptor = pod
	if _, err := rp.preemptionManager.PauseCandidate(ctx, candidate); err != nil {
		klog.ErrorS(err, "failed to pause pod on node", "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "failed to preempt candidate")
	}
	klog.InfoS("debug: successfully paused candidate pod", "candidate", klog.KObj(candidate.Pod), "node", candidate.NodeName)
	return framework.NewPostFilterResultWithNominatedNode(candidate.NodeName), framework.NewStatus(framework.Success)
}

func (rp *LLFPreemptiveScheduling) selectCandidate(candidates []*preemption.Candidate) *preemption.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	maxLaxity, _ := rp.laxityManager.GetPodLaxity(candidates[0].Pod)
	bestCandidate := candidates[0]
	for i := 1; i < len(candidates); i++ {
		c := candidates[i]
		laxity, _ := rp.laxityManager.GetPodLaxity(c.Pod)
		// TODO: remove debug log
		klog.InfoS("selectCandidate", "pod", klog.KObj(c.Pod), "laxity", laxity, "phase", c.Pod.Status.Phase, "node", c.NodeName)
		// ENDTODO
		if laxity > maxLaxity {
			maxLaxity = laxity
			bestCandidate = c
		}
	}
	return bestCandidate
}

func (rp *LLFPreemptiveScheduling) findCandidateOnNode(pod *v1.Pod, nodeInfo *framework.NodeInfo) *v1.Pod {
	maxLaxity, _ := rp.laxityManager.GetPodLaxity(pod)
	// TODO: remove debug log
	klog.InfoS("findCandidateOnNode", "pod2sched", klog.KObj(pod), "laxity", maxLaxity, "phase", pod.Status.Phase, "node", nodeInfo.Node().Name)
	//ENDTODO

	candidateLaxityQ := priorityqueue.New(0)
	candidatePods := make(map[string]*v1.Pod)
	var unpausedPods []*v1.Pod
	for _, podInfo := range append(nodeInfo.Pods, rp.fh.NominatedPodsForNode(nodeInfo.Node().Name)...) {
		p, err := rp.podLister.Pods(podInfo.Pod.Namespace).Get(podInfo.Pod.Name)
		if err != nil {
			klog.ErrorS(err, "Getting updated pod from node", "pod", klog.KRef(podInfo.Pod.Namespace, podInfo.Pod.Name), "node", nodeInfo.Node().Name)
			p = podInfo.Pod // fallback to pod from nodeInfo
		}
		if p.UID == pod.UID {
			klog.InfoS("skipping pod with the same uid", "p", klog.KObj(p), "pod", klog.KObj(pod))
			continue
		}
		if !rp.preemptionManager.CanBePaused(p) {
			klog.InfoS("skipping pod as pod cannot be paused", "p", klog.KObj(p), "pod", klog.KObj(pod))
			continue
		}
		if p.Namespace == util.NamespaceKubeSystem {
			klog.InfoS("skipping kube system pod", "p", klog.KObj(p), "pod", klog.KObj(pod))
			continue
		}
		if len(p.Spec.NodeName) == 0 {
			klog.InfoS("skipping pod not yet bound to a node", "p", klog.KObj(p), "pod", klog.KObj(pod))
			continue
		}
		if rp.preemptionManager.IsPodMarkedPaused(pod) || p.Status.Phase == v1.PodPaused {
			klog.InfoS("skipping paused/to-be-paused pod", "p", klog.KObj(p), "pod", klog.KObj(pod))
			continue
		}

		unpausedPods = append(unpausedPods, p)
		laxity, _ := rp.laxityManager.GetPodLaxity(p)
		// TODO: remove debug log
		klog.InfoS("findCandidateOnNode", "candidate", klog.KObj(p), "laxity", laxity, "phase", p.Status.Phase, "node", p.Spec.NodeName)
		// ENDTODO
		if laxity > maxLaxity {
			maxLaxity = laxity
			qItem := priorityqueue.NewItem(string(p.UID), int64(laxity))
			candidateLaxityQ.PushItem(qItem)
			candidatePods[qItem.Value()] = p
		}
	}
	if len(candidatePods) == 0 {
		// TODO change to level 4
		klog.InfoS("found no candidate pods on node with laxity later than current pod laxity", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
		// ENDTODO
		return nil
	}

	for i := 0; i < candidateLaxityQ.Size(); i++ {
		candidatePod := candidatePods[candidateLaxityQ.PopItem().Value()]
		// check if the preemptible pod is excluded it would yield enough resource to run the current pod
		var podsExcludeCandidate []*v1.Pod
		for _, p := range unpausedPods {
			if p.UID != candidatePod.UID {
				podsExcludeCandidate = append(podsExcludeCandidate, p)
			}
		}
		nodeAfterPreemption := framework.NewNodeInfo(podsExcludeCandidate...)
		nodeAfterPreemption.SetNode(nodeInfo.Node())
		insufficientResources := noderesources.Fits(pod, nodeAfterPreemption)
		if len(insufficientResources) > 0 {
			// TODO change to level 4
			klog.InfoS("there is not enough resource to run pod on node even after preemption", "errors", insufficientResources, "candidate", klog.KObj(candidatePod), "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
			// ENDTODO
			continue
		}
		return candidatePod
	}
	return nil
}
