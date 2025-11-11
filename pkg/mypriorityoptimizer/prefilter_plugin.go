// prefilter_plugin.go

package mypriorityoptimizer

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
func (pl *MyPriorityOptimizer) PreFilter(ctx context.Context, st *framework.CycleState, pending *v1.Pod) (*framework.PreFilterResult, *framework.Status) {

	stage := "PreFilter"

	ap := pl.getActivePlan()

	// Always allow kube-system pods and when no active plan exists.
	if pending.Namespace == SystemNamespace || ap == nil {
		return nil, framework.NewStatus(framework.Success)
	}

	allowed, allowedMsg, ok := pl.allowedNodes(pending)

	var nodes []string
	if allowed != nil {
		nodes = allowed.UnsortedList()
	}
	klog.V(MyV).InfoS(msg(stage, "filter decision"),
		"activePlan", ap != nil,
		"pod", combineNsName(pending.Namespace, pending.Name),
		"nodes", nodes,
		"reason", allowedMsg,
	)

	switch {
	case ok && allowed == nil:
		klog.V(MyV).InfoS(msg(stage, InfoAllowPod), "pod", klog.KObj(pending), "reason", allowedMsg)
		return nil, framework.NewStatus(framework.Success)

	case ok && allowed.Len() > 0:
		klog.V(MyV).InfoS(msg(stage, InfoPinPod), "pod", klog.KObj(pending), "nodes", nodes, "reason", allowedMsg)
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)

	default:
		klog.V(MyV).InfoS(msg(stage, InfoBlockPod), "pod", klog.KObj(pending), "reason", allowedMsg)
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, msg(stage, allowedMsg))
	}
}

func (pl *MyPriorityOptimizer) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

var _ framework.PreFilterPlugin = &MyPriorityOptimizer{}
