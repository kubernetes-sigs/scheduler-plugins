package rtpreemptive

import (
	"k8s.io/apimachinery/pkg/runtime"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/clock"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/laxity"
)

const (
	// NameLLF is the name of the plugin used in the plugin registry and configuration
	NameLLF = "LLFPreemptiveScheduling"
)

var (
	// sort pods based on laxity
	_ framework.QueueSortPlugin = &LLFPreemptiveScheduling{}
	// _ framework.PreFilterPlugin = &LLFPreemptiveScheduling{}
	// _ framework.FilterPlugin = &LLFPreemptiveScheduling{}
	// _ framework.PostFilterPlugin = &LLFPreemptiveScheduling{}
)

// LLFPreemptiveScheduling implements several plugins to perform soft real-time
// least laxity first scheduling
type LLFPreemptiveScheduling struct {
	fh              framework.Handle
	deadlineManager deadline.Manager
	laxityManager   laxity.Manager
	clock           clock.Clock
}

// Name returns name of the plugin, It is used in logs, etc.
func (rp *LLFPreemptiveScheduling) Name() string {
	return NameLLF
}

// NewEDF initializes a new LLFPreemptiveScheduling plugin and return it.
func NewLLF(_ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	deadlineManager := deadline.NewDeadlineManager()
	return &LLFPreemptiveScheduling{
		fh:              fh,
		deadlineManager: deadlineManager,
		laxityManager:   laxity.NewLaxityManager(deadlineManager),
		clock:           clock.RealClock{},
	}, nil
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
