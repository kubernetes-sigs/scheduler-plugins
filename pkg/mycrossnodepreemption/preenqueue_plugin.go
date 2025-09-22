// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreEnqueue is called before a pod is enqueued for scheduling.
func (pl *MyCrossNodePreemption) PreEnqueue(ctx context.Context, pod *v1.Pod) *framework.Status {

	phaseLabel := "PreEnqueue"

	// Always allow kube-system pods
	if pod.Namespace == SystemNamespace {
		return framework.NewStatus(framework.Success)
	}

	// If caches are not warm, block the pod
	if !pl.CachesWarm.Load() {
		pl.BlockedWhileActive.AddPod(pod)
		klog.V(MyV).Info(phaseLabel + ": Caches not warmed up yet; skipping plugin logic")
		return framework.NewStatus(framework.Pending, phaseLabel+": Caches not warmed up yet; skipping plugin logic")
	}

	// Just prune on every PreEnqueue call
	_ = pl.pruneSet(pl.BlockedWhileActive, "Blocked")

	// Decide strategy for this pod
	switch pl.decideStrategy(PhasePreEnqueue) {

	case DecidePass:
		klog.V(MyV).InfoS(phaseLabel+": pass-through", "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)

	case DecideBatch:
		klog.V(MyV).InfoS(phaseLabel+": "+InfoBatchPod, "pod", klog.KObj(pod))
		pl.Batched.AddPod(pod)
		return framework.NewStatus(framework.Pending, phaseLabel+": "+InfoBatchPod)

	case DecideBlockWhileActive:
		if !pl.IsPodAllowedByActivePlan(pod) {
			klog.V(MyV).InfoS(phaseLabel+": "+InfoActivePlanInProgress+"; "+InfoBlockPod, "pod", klog.KObj(pod))
			pl.BlockedWhileActive.AddPod(pod)
			return framework.NewStatus(framework.Pending, phaseLabel+": "+InfoActivePlanInProgress+"; "+InfoBlockPod)
		}
		return framework.NewStatus(framework.Success) // fallback

	case DecideEvery:
		klog.InfoS(phaseLabel+": start", "pod", klog.KObj(pod))
		_, err := pl.runFlow(ctx, pod)
		if err != nil {
			switch err {
			case ErrActiveInProgress:
				pl.BlockedWhileActive.AddPod(pod)
				return framework.NewStatus(framework.Pending, phaseLabel+": "+InfoActivePlanInProgress)
			case ErrSolverFailed:
				pl.BlockedWhileActive.AddPod(pod)
				return framework.NewStatus(framework.Pending, phaseLabel+": "+InfoSolverFailed)
			default: // else ErrRegisterPlan
				return framework.NewStatus(framework.Pending, phaseLabel+": "+InfoRegisterPlanFailed)
			}
		}
		return framework.NewStatus(framework.Success, phaseLabel+": "+InfoNominatedAfterPlan)
	}

	return framework.NewStatus(framework.Success)
}
