// prefilter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreFilterPlugin = &MyCrossNodePreemption{}

// PreFilter is called at the beginning of scheduling cycle.
// It is used, here, to filter the node(s) that the pod can be (tried) scheduled on.
// If a pod part of a plan was scheduled on a wrong node due to workload quotas,
// it is determined in Reserve plugin and will be retried again.
func (pl *MyCrossNodePreemption) PreFilter(ctx context.Context, st *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	ap := pl.getActivePlan()
	if pod.Namespace == "kube-system" || ap == nil {
		return nil, framework.NewStatus(framework.Success)
	}
	nodes, msg, ok := pl.allowedNodes(pod)
	klog.V(MyVerbosity).InfoS("Filter decision",
		"activePlan", ap != nil,
		"pod", pod.Namespace+"/"+pod.Name,
		"nodes", nodes.UnsortedList())

	switch {
	case ok && nodes == nil:
		// allowed, no pin
		klog.V(MyVerbosity).InfoS("PreFilter: allow", "pod", klog.KObj(pod), "reason", msg)
		return nil, framework.NewStatus(framework.Success)
	case ok && nodes.Len() > 0:
		klog.V(MyVerbosity).InfoS("PreFilter: pin", "pod", klog.KObj(pod), "nodes", nodes.UnsortedList(), "reason", msg)
		return &framework.PreFilterResult{NodeNames: nodes}, framework.NewStatus(framework.Success)
	default: // not ok
		klog.V(MyVerbosity).InfoS("PreFilter: block", "pod", klog.KObj(pod), "reason", msg)
		pl.Blocked.AddPod(pod)
		return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: "+msg)
	}
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
