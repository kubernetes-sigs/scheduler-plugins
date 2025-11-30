// preenqueue_phase.go

package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreEnqueue is called before a pod is enqueued for scheduling.
func (pl *SharedState) PreEnqueue(ctx context.Context, pending *v1.Pod) *framework.Status {

	stage := "PreEnqueue"

	// Always allow kube-system pods
	if pending.Namespace == SystemNamespace {
		return framework.NewStatus(framework.Success)
	}

	// If caches are not warm, block the pod
	if !pl.CachesWarm.Load() {
		pl.BlockedWhileActive.AddPod(pending)
		klog.V(MyV).Info(msg(stage, "caches not warmed up yet; waiting"))
		return framework.NewStatus(framework.Pending, msg(stage, "caches not warmed up yet; waiting"))
	}

	// Decide strategy for this pod
	switch pl.decideStrategy(StagePreEnqueue) {

	case DecidePass:
		klog.V(MyV).InfoS(msg(stage, "pass-through"), "pod", klog.KObj(pending))
		return framework.NewStatus(framework.Success)

	case DecideProcessLater:
		klog.V(MyV).InfoS(msg(stage, InfoPendingPod), "pod", klog.KObj(pending))
		return framework.NewStatus(framework.Pending, msg(stage, InfoPendingPod))

	case DecideBlock:
		if !pl.isPodAllowedByActivePlan(pending) {
			klog.V(MyV).InfoS(msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod), "pod", klog.KObj(pending))
			pl.BlockedWhileActive.AddPod(pending)
			return framework.NewStatus(framework.Pending, msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod))
		}
		return framework.NewStatus(framework.Success) // fallback

	case DecideProcess:
		klog.InfoS(msg(stage, "start"), "pod", klog.KObj(pending))
		_, _, _, _, _, err := pl.runFlow(ctx, pending)
		if err != nil {
			switch err {
			case ErrActiveInProgress: // we only keep the pod in the set if we get ErrActiveInProgress
				pl.BlockedWhileActive.AddPod(pending)
				return framework.NewStatus(framework.Pending, msg(stage, InfoActivePlanInProgress))
			default: // else
				return framework.NewStatus(framework.Pending, msg(stage, InfoPlanRegistrationFailed))
			}
		}
		return framework.NewStatus(framework.Success, msg(stage, InfoNominatedAfterPlan))
	}

	return framework.NewStatus(framework.Success)
}
