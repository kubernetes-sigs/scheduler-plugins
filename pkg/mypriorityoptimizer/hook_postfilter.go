// postfilter_hook.go

package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostFilter is called after filtering a pod.
func (pl *SharedState) PostFilter(ctx context.Context, _ *framework.CycleState, pending *v1.Pod, _ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	stage := "PostFilter"

	// If active plan in progress, block the pod.
	ap := pl.getActivePlan()
	if ap != nil {
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoActivePlanInProgress))
	}

	switch pl.decideStrategy(StagePostFilter) {

	case DecidePass:
		return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoNoStrategyEnabled))

	case DecideProcessLater:
		return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoPendingPod))

	case DecideBlock:
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoActivePlanInProgress))

	case DecideProcess:
		klog.InfoS(msg(stage, "start"), "pod", klog.KObj(pending))
		plan, _, _, _, _, err := pl.runFlow(ctx, pending)
		if err != nil {
			switch err {
			case ErrActiveInProgress: // we only keep the pod in the set if we get ErrActiveInProgress
				pl.BlockedWhileActive.AddPod(pending)
				return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoActivePlanInProgress))
			default: // else
				return nil, framework.NewStatus(framework.Unschedulable, msg(stage, InfoPlanRegistrationFailed))
			}
		}

		// Return the result with the nominated node information which the scheduler will use to bind the pod.
		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.NominatedNode, NominatingMode: framework.ModeOverride},
		}, framework.NewStatus(framework.Success, msg(stage, InfoNominatedAfterPlan))

	default:
		klog.Error(msg(stage, "unexpected decision"))
		return nil, framework.NewStatus(framework.Error, msg(stage, "unexpected decision"))
	}
}
