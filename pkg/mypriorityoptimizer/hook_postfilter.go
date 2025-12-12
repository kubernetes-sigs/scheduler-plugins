// hook_postfilter.go
package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var postFilterSleep = time.Sleep

// -------------------------
// PostFilter
// --------------------------

// PostFilter is called after filtering a pod.
func (pl *SharedState) PostFilter(ctx context.Context, state fwk.CycleState, pending *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *fwk.Status) {
	stage := "PostFilter"

	// Only proceed if PerPod is enabled; otherwise, just skip.
	if !isPerPodMode() {
		klog.V(MyV).InfoS(msg(stage, "no nomination"), "pod", klog.KObj(pending))
		return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, "no nomination"))
	}

	// If active plan in progress, block the pod.
	ap := pl.getActivePlan()
	if ap != nil {
		klog.V(MyV).InfoS(msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod), "pod", klog.KObj(pending))
		pl.BlockedWhileActive.AddPodSafely(pending)
		return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoActivePlanInProgress))
	}

	// If no active plan, run optimisation flow for the pod.
	klog.InfoS(msg(stage, "start"), "pod", klog.KObj(pending))
	postFilterSleep(1 * time.Second) // TODO: little hack to ensure other concurrent workers has time to finish their work such that we have a reliable view of the cluster state
	plan, _, _, _, _, err := pl.runOptimizationFlow(ctx, pending)
	if err != nil {
		switch err {
		case ErrActiveInProgress: // we only keep the pod in the set if we get ErrActiveInProgress
			pl.BlockedWhileActive.AddPodSafely(pending)
			return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoActivePlanInProgress))
		default: // else
			return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoPlanRegistrationFailed))
		}
	}

	// Return the result with the nominated node information which the scheduler
	// will use to bind the pod.
	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.NominatedNode, NominatingMode: framework.ModeOverride},
	}, fwk.NewStatus(fwk.Success, msg(stage, InfoNominatedAfterPlan))
}
