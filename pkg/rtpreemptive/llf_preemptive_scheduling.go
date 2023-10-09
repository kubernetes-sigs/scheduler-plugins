package rtpreemptive

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/clock"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
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
	// _ framework.PreFilterPlugin = &LLFPreemptiveScheduling{}
	// _ framework.FilterPlugin = &LLFPreemptiveScheduling{}
	// _ framework.PostFilterPlugin = &LLFPreemptiveScheduling{}
)

// LLFPreemptiveScheduling implements several plugins to perform soft real-time
// least laxity first scheduling
type LLFPreemptiveScheduling struct {
	fh                framework.Handle
	deadlineManager   deadline.Manager
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
	deadlineManager := deadline.NewDeadlineManager()
	laxityManager := laxity.NewLaxityManager(deadlineManager)
	prioFunc := func(p *v1.Pod) int64 {
		laxity, _ := laxityManager.GetPodLaxity(p)
		return -int64(laxity)
	}
	llfSched := &LLFPreemptiveScheduling{
		fh:                fh,
		deadlineManager:   deadlineManager,
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
