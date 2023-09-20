package rtpreemptive

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
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
	// pause pod to be preempted or resume a paused pod and reject the current one
	// TODO: for starting point, assume tasks have equal priority
	_ framework.PostFilterPlugin = &EDFPreemptiveScheduling{}
	// // rank node based total laxity, the higher the better (more jobs are allowed to be delayed)
	// _ framework.ScorePlugin = &EDFPreemptiveScheduling{}
	// // implement preemption by update pod instead of delete pod
	// _ preemption.Interface = &EDFPreemptiveScheduling{}
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
func New(fh framework.Handle) (framework.Plugin, error) {
	return &EDFPreemptiveScheduling{
		fh:                fh,
		podLister:         fh.SharedInformerFactory().Core().V1().Pods().Lister(),
		deadlineManager:   deadline.NewDeadlineManager(),
		preemptionManager: preemption.NewPreemptionManager(fh.SharedInformerFactory().Core().V1().Nodes().Lister()),
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
	if toPause := rp.preemptionManager.IsPodMarkedPaused(pod); toPause {
		return nil, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Pod %v/%v is rejected because pod is marked to be paused", pod.Namespace, pod.Name))
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

// PostFilter plugin is called when no node is found schedulable at Filter stage
// here it attempt to preempt a pod on available nodes
// if preemption is successful, return with nominated node and add pod to paused map
func (rp *EDFPreemptiveScheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	return nil, nil
}

func (rp *EDFPreemptiveScheduling) getPreemptiblePod(pod *v1.Pod, nodeInfo *framework.NodeInfo) *v1.Pod {
	podDDL := rp.deadlineManager.GetPodDeadline(pod)
	maxDDL := podDDL

	var preemptiblePod *v1.Pod
	var runningPods []*v1.Pod
	// find latest deadline among all running pods and update preemptiblePod accordingly
	for _, podInfo := range nodeInfo.Pods {
		p, err := rp.podLister.Pods(podInfo.Pod.Namespace).Get(podInfo.Pod.Name)
		if err != nil {
			klog.ErrorS(err, "Getting updated pod from node", "pod", klog.KRef(podInfo.Pod.Namespace, podInfo.Pod.Name), "node", nodeInfo.Node().Name)
			p = podInfo.Pod // fallback to pod from nodeInfo
		}
		if p.Status.Phase == v1.PodRunning {
			runningPods = append(runningPods, p)
			ddl := rp.deadlineManager.GetPodDeadline(p)
			if ddl.After(maxDDL) {
				maxDDL = ddl
				preemptiblePod = p
			}
		}
	}
	if preemptiblePod == nil {
		klog.InfoS("found no pods on node with deadline later than current pod deadline", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
		return nil
	}

	// check if the preemptible pod is excluded it would yield enough resource to run the current pod
	var nonePreemptiblePods []*v1.Pod
	for _, p := range runningPods {
		ddl := rp.deadlineManager.GetPodDeadline(p)
		if ddl.Before(maxDDL) {
			nonePreemptiblePods = append(nonePreemptiblePods, p)
		}
	}
	nodeAfterPreemption := framework.NewNodeInfo(nonePreemptiblePods...)
	insufficientResources := noderesources.Fits(pod, nodeAfterPreemption)
	if len(insufficientResources) > 0 {
		klog.InfoS("even after preemption, there is not enough resource to run pod on node", "pod", klog.KObj(pod), "node", klog.KObj(nodeInfo.Node()))
		return nil
	}
	return preemptiblePod
}
