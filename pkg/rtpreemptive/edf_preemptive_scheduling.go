package rtpreemptive

import (
	"context"
	"errors"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/utils/clock"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/preemption"
)

const (
	// Name of the plugin used in the plugin registry and configuration
	Name = "EDFPreemptiveScheduling"
	// used in cycle state
	PodDeadlinesSnapshotKey = Name + "/PodDeadlinesSnapshot"
	PreemptiblePodsKey      = Name + "/PreemptiblePods"
)

var (
	// sort pods based on priority and absolute deadline
	_ framework.QueueSortPlugin = &EDFPreemptiveScheduling{}
	// check if pod is marked as paused, set pod as pending
	_ framework.PreFilterPlugin = &EDFPreemptiveScheduling{}
	// check if pod is marked as paused, set pod as pending
	_ framework.FilterPlugin = &EDFPreemptiveScheduling{}
	// pause pod to be preempted or resume a paused pod and reject the current one
	// TODO: for starting point, assume tasks have equal priority
	_ framework.PostFilterPlugin = &EDFPreemptiveScheduling{}
	// // rank node based total laxity, the higher the better (more jobs are allowed to be delayed)
	// _ framework.ScorePlugin = &EDFPreemptiveScheduling{}
)

// EDFPreemptiveScheduling implements several plugins to perform soft real-time
// (paused-/resume-based) preemptive scheduling
type EDFPreemptiveScheduling struct {
	fh                framework.Handle
	podLister         corelisters.PodLister
	deadlineManager   deadline.Manager
	preemptionManager preemption.Manager
	clock             clock.Clock
}

// Name returns name of the plugin, It is used in logs, etc.
func (rp *EDFPreemptiveScheduling) Name() string {
	return Name
}

// New initializes a new plugin and return it.
func New(_ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	podLister := fh.SharedInformerFactory().Core().V1().Pods().Lister()
	nodeLister := fh.SharedInformerFactory().Core().V1().Nodes().Lister()
	nodeInfoLister := fh.SnapshotSharedLister().NodeInfos()
	return &EDFPreemptiveScheduling{
		fh:                fh,
		podLister:         podLister,
		deadlineManager:   deadline.NewDeadlineManager(),
		preemptionManager: preemption.NewPreemptionManager(podLister, nodeLister, nodeInfoLister, fh.ClientSet()),
		clock:             clock.RealClock{},
	}, nil
}

// QueueSort Plugin
// Less is used to sort pods in the scheduling queue in the following order.
//  1. Compare the priorities of Pods.
//  2. Compare the absolute deadline based on pod creation time and relative deadline defined in annotations.
//     If the creation time is not set, use current timestamp as fallback.
//     If the relative deadline is not defined via 'simpleddl.scheduling.x-k8s.io/ddl' annotations, use 10m as fallback
//  3. Compare the keys of Pods: <namespace>/<podname>.
func (rp *EDFPreemptiveScheduling) Less(podInfo1, podInfo2 *framework.QueuedPodInfo) bool {
	prio1 := corev1helpers.PodPriority(podInfo1.Pod)
	prio2 := corev1helpers.PodPriority(podInfo2.Pod)
	if prio1 != prio2 {
		return prio1 > prio2
	}
	ddl1 := rp.deadlineManager.AddPodDeadline(podInfo1.Pod)
	ddl2 := rp.deadlineManager.AddPodDeadline(podInfo2.Pod)
	if ddl1.Equal(ddl2) {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}
	return ddl1.Before(ddl2)
}

// PreFilter Plugin
// checks if the pod to schedule is already marked as paused,
// if so let it to fail scheduling
func (rp *EDFPreemptiveScheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// FIXME, a pod marked to be paused may not have been paused yet
	// we either make the assumption that we cannot pass this metadata from yaml
	// or we need to find a way to find out this was marked by scheduler instead of a user
	// or find a way to not differentiate those two and still make it work?
	if toPause := rp.preemptionManager.IsPodMarkedPaused(pod); pod.Status.Phase != v1.PodPaused && toPause {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("Pod %v/%v is rejected because pod is marked to be paused", pod.Namespace, pod.Name))
	}
	rp.deadlineManager.AddPodDeadline(pod)
	return nil, framework.NewStatus(framework.Success, "")
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (rp *EDFPreemptiveScheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return rp
}

// AddPod implements PreFilterExtensions AddPod
func (rp *EDFPreemptiveScheduling) AddPod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	rp.deadlineManager.AddPodDeadline(podInfoToAdd.Pod)
	return framework.NewStatus(framework.Success, "")
}

// AddPod implements PreFilterExtensions RemovePod
func (rp *EDFPreemptiveScheduling) RemovePod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	rp.deadlineManager.RemovePodDeadline(podInfoToRemove.Pod)
	return framework.NewStatus(framework.Success, "")
}

func (rp *EDFPreemptiveScheduling) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	// TODO: resume pod should also take into consideration of the node
	// when a pod need to be resumed is on another, then we could schedule the pod else where
	// but we need handle the edge case when the pod to be scheduled is the paused pod itself which need to be resumed
	// in this case, we might need to use preFilter to mark those nodes unschedulable and unresolvable
	//
	// So, if its the paused pod itself that need to be resumed, we skip the rest scheduling all together
	// Otherwise, loop through all nodes, and find if there are pods on that node need to be resumed
	// if so, we mark that node as unschedulable and unresolvable
	//
	// Perhaps we need to handle paused pod in pre-filter
	if len(pod.Spec.NodeName) > 0 && pod.Spec.NodeName != nodeInfo.Node().Name {
		// pod is already assigned to a node and it's not the same as given node
		// this happens when a paused pod re-enters the scheduling queue
		return framework.NewStatus(framework.UnschedulableAndUnresolvable)
	}
	if candidate := rp.preemptionManager.ResumePausedPod(ctx, pod); candidate != nil {
		if candidate.Pod.UID == pod.UID {
			klog.InfoS("pod was paused and now is resumed", "pod", klog.KObj(pod), "node", candidate.NodeName)
			return framework.NewStatus(framework.Success, "pod was paused and now is resumed")
		}
		klog.ErrorS(errors.New("pod unschedulable"), "a paused pod need to be resumed", "podToResume", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "a paused pod is resumed instead")
	}
	var unpausedPods []*v1.Pod
	for _, pod := range nodeInfo.Pods {
		if pod.Pod.Status.Phase != v1.PodPaused {
			klog.InfoS(">>>> Filter: found unpaused pod", "pod", klog.KObj(pod.Pod), "node", klog.KObj(nodeInfo.Node()))
			unpausedPods = append(unpausedPods, pod.Pod)
		}
	}
	nodeWithExcludePausedPods := framework.NewNodeInfo(unpausedPods...)
	nodeWithExcludePausedPods.SetNode(nodeInfo.Node())
	insufficientResources := noderesources.Fits(pod, nodeWithExcludePausedPods)
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
func (rp *EDFPreemptiveScheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	allNode, err := rp.fh.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "failed to list all nodes", "pod", klog.KObj(pod))
		return nil, framework.AsStatus(err)
	}
	var candidates []*preemption.Candidate
	for _, nodeInfo := range allNode {
		// skip node where preemption is not helpful
		if filteredNodeStatusMap[nodeInfo.Node().Name].Code() == framework.UnschedulableAndUnresolvable {
			klog.InfoS("skipping unschedulable and unresolvable node", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
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
	klog.InfoS("found candidate pod to pause on node", "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
	if err := rp.preemptionManager.PauseCandidate(ctx, candidate); err != nil {
		klog.ErrorS(err, "failed to pause pod on node", "candidate", klog.KObj(candidate.Pod), "pod", klog.KObj(pod), "node", candidate.NodeName)
		return nil, framework.AsStatus(err)
	}
	return framework.NewPostFilterResultWithNominatedNode(candidate.NodeName), framework.NewStatus(framework.Success)
}

func (rp *EDFPreemptiveScheduling) selectCandidate(candidates []*preemption.Candidate) *preemption.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	maxDDL := rp.deadlineManager.GetPodDeadline(candidates[0].Pod)
	bestCandidate := candidates[0]
	for _, c := range candidates {
		ddl := rp.deadlineManager.GetPodDeadline(c.Pod)
		if ddl.After(maxDDL) {
			maxDDL = ddl
			bestCandidate = c
		}
	}
	return bestCandidate
}

func (rp *EDFPreemptiveScheduling) findCandidateOnNode(pod *v1.Pod, nodeInfo *framework.NodeInfo) *v1.Pod {
	podDDL := rp.deadlineManager.GetPodDeadline(pod)
	maxDDL := podDDL

	var candidatePod *v1.Pod
	var unpausedPods []*v1.Pod
	// find latest deadline among all running pods and update candidate accordingly
	for _, podInfo := range nodeInfo.Pods {
		p, err := rp.podLister.Pods(podInfo.Pod.Namespace).Get(podInfo.Pod.Name)
		if err != nil {
			klog.ErrorS(err, "Getting updated pod from node", "pod", klog.KRef(podInfo.Pod.Namespace, podInfo.Pod.Name), "node", nodeInfo.Node().Name)
			p = podInfo.Pod // fallback to pod from nodeInfo
		}
		klog.InfoS("checking pods", "pod", klog.KObj(p), "phase", p.Status.Phase, "node", klog.KObj(nodeInfo.Node()))
		if p.Status.Phase != v1.PodPaused {
			unpausedPods = append(unpausedPods, p)
			ddl := rp.deadlineManager.GetPodDeadline(p)
			if ddl.After(maxDDL) {
				maxDDL = ddl
				candidatePod = p
			}
		}
	}
	if candidatePod == nil {
		klog.InfoS("found no candidate pods on node with deadline later than current pod deadline", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
		return nil
	}

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
		klog.InfoS("there is not enough resource to run pod on node even after preemption", "candidate", klog.KObj(candidatePod), "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
		return nil
	}
	return candidatePod
}
