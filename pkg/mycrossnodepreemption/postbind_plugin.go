// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind runs after a pod is bound by the scheduler.
// When the preemptor binds and our plan goals are satisfied,
// we mark the plan completed and clear the "blocked" set.
// Requeueing of previously blocked pods is then driven by our
// QueueingHintFn registered on Pod/ConfigMap events.
func (pl *MyCrossNodePreemption) PostBind(
	ctx context.Context,
	_ *framework.CycleState,
	_ *v1.Pod,
	_ string,
) {
	sp, planID := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return
	}

	ok, err := pl.isPlanCompleted(ctx, sp)
	if err != nil {
		pl.onPlanSettled()
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(2).InfoS("PostBind: plan still active")
		return
	}

	if pl.onPlanSettled() {
		pl.markPlanCompleted(ctx, planID) // safe to call after; it’s idempotent
	}
}
