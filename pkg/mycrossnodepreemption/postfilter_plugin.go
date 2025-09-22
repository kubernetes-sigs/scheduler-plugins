// postfilter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostFilter is called after filtering a pod.
// It is a replacement for the default preemption with cross-node preemption logic implemented.
// It catch all pods not handled by the default scheduling.
func (pl *MyCrossNodePreemption) PostFilter(ctx context.Context, _ *framework.CycleState, pending *v1.Pod, _ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	phase := "PostFilter"

	// If active plan in progress, block the pod.
	ap := pl.getActivePlan()
	if ap != nil {
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, phaseMsg(phase, InfoActivePlanInProgress))
	}

	switch pl.decideStrategy(PhasePostFilter) {

	case DecidePass:
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, phaseMsg(phase, InfoNoStrategyEnabled))

	case DecidePending:
		klog.V(MyV).InfoS(phaseMsg(phase, InfoPendingPod), "pod", klog.KObj(pending))
		return nil, framework.NewStatus(framework.Unschedulable, phaseMsg(phase, InfoPendingPod))

	case DecideBlockWhileActive:
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, phaseMsg(phase, InfoActivePlanInProgress))

	case DecideEvery:
		klog.InfoS(phaseMsg(phase, "start"), "pod", klog.KObj(pending))
		targetNode, err := pl.runFlow(ctx, pending)
		if err != nil {
			switch err {
			case ErrActiveInProgress: // we only keep the pod in the set if we get ErrActiveInProgress
				pl.BlockedWhileActive.AddPod(pending)
				return nil, framework.NewStatus(framework.Unschedulable, phaseMsg(phase, InfoActivePlanInProgress))
			default: // else
				return nil, framework.NewStatus(framework.Unschedulable, phaseMsg(phase, InfoRegisterPlanFailed))
			}
		}

		// Return the result with the nominated node information which the scheduler will use to bind the pod.
		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{NominatedNodeName: targetNode, NominatingMode: framework.ModeOverride},
		}, framework.NewStatus(framework.Success, phaseMsg(phase, InfoNominatedAfterPlan))

	default:
		klog.Error(phaseMsg(phase, "unexpected decision"))
		return nil, framework.NewStatus(framework.Error, phaseMsg(phase, "unexpected decision"))
	}
}
