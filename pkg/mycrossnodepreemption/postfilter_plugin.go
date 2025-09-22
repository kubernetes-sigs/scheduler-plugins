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

	phaseLabel := "PostFilter"

	// If active plan in progress, block the pod.
	ap := pl.getActivePlan()
	if ap != nil {
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoActivePlanInProgress)
	}

	// Prune BlockedWhileActive set to remove deleted/assigned pods.
	_ = pl.pruneSet(pl.BlockedWhileActive)

	switch pl.decideStrategy(PhasePostFilter) {

	case DecidePass:
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, phaseLabel+": "+InfoNoStrategyEnabled)

	case DecideBatch:
		klog.V(MyV).InfoS(phaseLabel+": "+InfoBatchPod, "pod", klog.KObj(pending))
		pl.Batched.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoBatchPod)

	case DecideBlockWhileActive:
		pl.BlockedWhileActive.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoActivePlanInProgress)

	case DecideEvery:
		klog.InfoS(phaseLabel+": start", "pod", klog.KObj(pending))
		targetNode, err := pl.runFlow(ctx, pending)
		if err != nil {
			if err == ErrActiveInProgress {
				pl.BlockedWhileActive.AddPod(pending)
				return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoActivePlanInProgress)
			}
			if err == ErrSolverFailed {
				return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoSolverFailed)
			}
			// Else ErrRegisterPlan
			return nil, framework.NewStatus(framework.Unschedulable, phaseLabel+": "+InfoRegisterPlanFailed)
		}

		// Return the result with the nominated node information which the scheduler will use to bind the pod.
		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{NominatedNodeName: targetNode, NominatingMode: framework.ModeOverride},
		}, framework.NewStatus(framework.Success, phaseLabel+": "+InfoNominatedAfterPlan)

	default:
		klog.Error(phaseLabel + ": unexpected decision")
		return nil, framework.NewStatus(framework.Error, phaseLabel+": unexpected decision")
	}
}
