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

	phase := "PreEnqueue"

	// Always allow kube-system pods
	if pod.Namespace == SystemNamespace {
		return framework.NewStatus(framework.Success)
	}

	// If caches are not warm, block the pod
	if !pl.CachesWarm.Load() {
		pl.BlockedWhileActive.AddPod(pod)
		klog.V(MyV).Info(msg(phase, "caches not warmed up yet; waiting"))
		return framework.NewStatus(framework.Pending, msg(phase, "caches not warmed up yet; waiting"))
	}

	// Decide strategy for this pod
	switch pl.decideStrategy(PhasePreEnqueue) {

	case DecidePass:
		klog.V(MyV).InfoS(msg(phase, "pass-through"), "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)

	case DecidePending:
		klog.V(MyV).InfoS(msg(phase, InfoPendingPod), "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Pending, msg(phase, InfoPendingPod))

	case DecideBlockWhileActive:
		if !pl.IsPodAllowedByActivePlan(pod) {
			klog.V(MyV).InfoS(msg(phase, InfoActivePlanInProgress+"; "+InfoBlockPod), "pod", klog.KObj(pod))
			pl.BlockedWhileActive.AddPod(pod)
			return framework.NewStatus(framework.Pending, msg(phase, InfoActivePlanInProgress+"; "+InfoBlockPod))
		}
		return framework.NewStatus(framework.Success) // fallback

	case DecideEvery:
		klog.InfoS(msg(phase, "start"), "pod", klog.KObj(pod))
		_, err := pl.runFlow(ctx, pod)
		if err != nil {
			switch err {
			case ErrActiveInProgress: // we only keep the pod in the set if we get ErrActiveInProgress
				pl.BlockedWhileActive.AddPod(pod)
				return framework.NewStatus(framework.Pending, msg(phase, InfoActivePlanInProgress))
			default: // else
				return framework.NewStatus(framework.Pending, msg(phase, InfoRegisterPlanFailed))
			}
		}
		return framework.NewStatus(framework.Success, msg(phase, InfoNominatedAfterPlan))
	}

	return framework.NewStatus(framework.Success)
}
