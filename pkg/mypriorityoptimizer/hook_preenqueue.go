// hook_preenqueue.go
package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
)

// -------------------------
// PreEnqueue
// --------------------------

// PreEnqueue is called before a pod is enqueued for scheduling.
func (pl *SharedState) PreEnqueue(ctx context.Context, pending *v1.Pod) *fwk.Status {
	const stage = "PreEnqueue"

	// 1) Always allow kube-system pods.
	if isPodProtected(pending) {
		return fwk.NewStatus(fwk.Success)
	}

	// 2) If caches are not warm, block the pod. It will be re-queued when ready.
	if !pl.PluginReady.Load() {
		pl.BlockedWhileActive.AddPodSafely(pending)
		klog.V(MyV).Info(msg(stage, "caches not warmed up yet; waiting"))
		return fwk.NewStatus(fwk.Pending, msg(stage, "caches not warmed up yet; waiting"))
	}

	// 3) If there is an active plan, enforce it.
	if ap := pl.getActivePlan(); ap != nil {
		if !pl.isPodAllowedByPlan(pending) {
			// Plan exists and pod is NOT allowed by the plan → block.
			pl.BlockedWhileActive.AddPodSafely(pending)
			klog.V(MyV).InfoS(
				msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod),
				"pod", klog.KObj(pending),
			)
			return fwk.NewStatus(fwk.Pending, msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod))
		}

		// Plan exists and pod IS allowed by the plan → let it through.
		klog.V(MyV).InfoS(
			msg(stage, "allowed by active plan; pass-through"),
			"pod", klog.KObj(pending),
		)
		return fwk.NewStatus(fwk.Success)
	}

	// 4) No active plan:

	//	- In Manual Blocking mode, block the pod to accumulate work for the solver.
	if isManualBlockingMode() {
		klog.V(MyV).InfoS(msg(stage, InfoPendingPod), "pod", klog.KObj(pending))
		// If you also want to track these as "blocked" for later unblocking,
		// uncomment the next line:
		// pl.BlockedWhileActive.AddPod(pending)
		return fwk.NewStatus(fwk.Pending, msg(stage, InfoPendingPod))
	}

	//	- Other modes, just let the pod through.
	klog.V(MyV).InfoS(msg(stage, "pass-through"), "pod", klog.KObj(pending))
	return fwk.NewStatus(fwk.Success)
}
