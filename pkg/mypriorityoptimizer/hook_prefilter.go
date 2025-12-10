// prefilter_hook.go

package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreFilter is called at the beginning of scheduling cycle.
// It is used, here, to filter the node(s) that the pod can be (tried) scheduled on.
// If a pod part of a plan was scheduled on a wrong node due to workload quotas,
// it is determined in Reserve plugin and will be retried again.
func (pl *SharedState) PreFilter(ctx context.Context, st fwk.CycleState, pending *v1.Pod, nodes []fwk.NodeInfo) (*framework.PreFilterResult, *fwk.Status) {

	stage := "PreFilter"

	ap := pl.getActivePlan()

	// Always allow kube-system pods and pending pods when no active plan exists.
	if isPodProtected(pending) || ap == nil {
		return nil, fwk.NewStatus(fwk.Success)
	}

	// Get filtering decision from active plan (if any)
	filteredNodes, filterMsg, ok := pl.filterNodes(pending)

	// Convert filteredNodes to slice for logging
	var nodeNames []string
	if filteredNodes != nil {
		nodeNames = filteredNodes.UnsortedList()
	}
	klog.V(MyV).InfoS(msg(stage, "filter decision"),
		"activePlan", ap != nil,
		"pod", mergeNsName(pending.Namespace, pending.Name),
		"nodes", nodeNames,
		"reason", filterMsg,
	)

	switch {
	case ok && filteredNodes == nil: // allowed on all nodes
		klog.V(MyV).InfoS(msg(stage, InfoAllowPod),
			"pod", klog.KObj(pending),
			"reason", filterMsg,
		)
		return nil, fwk.NewStatus(fwk.Success)

	case ok && filteredNodes.Len() > 0: // allowed on specific nodes
		klog.V(MyV).InfoS(msg(stage, InfoPinPod),
			"pod", klog.KObj(pending),
			"nodes", nodeNames,
			"reason", filterMsg,
		)
		return &framework.PreFilterResult{NodeNames: filteredNodes}, fwk.NewStatus(fwk.Success)

	default: // not allowed on any node; block the pod
		klog.V(MyV).InfoS(msg(stage, InfoBlockPod),
			"pod", klog.KObj(pending),
			"reason", filterMsg,
		)
		pl.BlockedWhileActive.AddPod(pending)
		return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, filterMsg))
	}
}

func (pl *SharedState) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

var _ framework.PreFilterPlugin = &SharedState{}
