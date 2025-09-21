// prefilter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreFilter is called at the beginning of scheduling cycle.
// It is used, here, to filter the node(s) that the pod can be (tried) scheduled on.
// If a pod part of a plan was scheduled on a wrong node due to workload quotas,
// it is determined in Reserve plugin and will be retried again.
func (pl *MyCrossNodePreemption) PreFilter(ctx context.Context, st *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {

	ap := pl.getActivePlan()

	// Always allow kube-system pods and when no active plan exists.
	if pod.Namespace == "kube-system" || ap == nil {
		return nil, framework.NewStatus(framework.Success)
	}

	// Get the allowed allowedNodes for this pod.
	allowedNodes, msg, ok := pl.allowedNodes(pod)
	klog.V(MyV).InfoS("PreFilter: filter decision", "activePlan", ap != nil, "pod", combineNsName(pod.Namespace, pod.Name), "nodes", allowedNodes.UnsortedList())
	switch {
	case ok && allowedNodes == nil:
		// allowed, no pin
		klog.V(MyV).InfoS("PreFilter: allow", "pod", klog.KObj(pod), "reason", msg)
		return nil, framework.NewStatus(framework.Success)
	case ok && allowedNodes.Len() > 0:
		klog.V(MyV).InfoS("PreFilter: pin", "pod", klog.KObj(pod), "nodes", allowedNodes.UnsortedList(), "reason", msg)
		return &framework.PreFilterResult{NodeNames: allowedNodes}, framework.NewStatus(framework.Success)
	default: // not ok
		klog.V(MyV).InfoS("PreFilter: block", "pod", klog.KObj(pod), "reason", msg)
		pl.Blocked.AddPod(pod)
		return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: "+msg)
	}
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

var _ framework.PreFilterPlugin = &MyCrossNodePreemption{}
