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

	phaseLabel := "PreFilter"

	ap := pl.getActivePlan()

	// Always allow kube-system pods and when no active plan exists.
	if pod.Namespace == SystemNamespace || ap == nil {
		return nil, framework.NewStatus(framework.Success)
	}

	allowed, msg, ok := pl.allowedNodes(pod)

	var nodes []string
	if allowed != nil {
		nodes = allowed.UnsortedList()
	}
	klog.V(MyV).InfoS(phaseLabel+": filter decision",
		"activePlan", ap != nil,
		"pod", combineNsName(pod.Namespace, pod.Name),
		"nodes", nodes,
		"reason", msg,
	)

	switch {
	case ok && allowed == nil:
		klog.V(MyV).InfoS(phaseLabel+": "+InfoAllowPod, "pod", klog.KObj(pod), "reason", msg)
		return nil, framework.NewStatus(framework.Success)

	case ok && allowed.Len() > 0:
		klog.V(MyV).InfoS(phaseLabel+": "+InfoPinPod, "pod", klog.KObj(pod), "nodes", nodes, "reason", msg)
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)

	default:
		klog.V(MyV).InfoS(phaseLabel+": "+InfoBlockPod, "pod", klog.KObj(pod), "reason", msg)
		pl.BlockedWhileActive.AddPod(pod)
		return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+msg)
	}
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

var _ framework.PreFilterPlugin = &MyCrossNodePreemption{}
