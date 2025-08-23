// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (pl *MyCrossNodePreemption) PostBind(ctx context.Context, _ *framework.CycleState, _ *v1.Pod, _ string) {
	sp, planID := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return
	}

	ok, err := pl.isPlanCompleted(ctx, sp)
	if err != nil {
		pl.onPlanSettled()
		klog.ErrorS(err, "completion check failed")
		return
	}
	if !ok {
		klog.V(2).InfoS("plan still active")
		return
	}

	if pl.onPlanSettled() {
		pl.markPlanCompleted(ctx, planID) // idempotent
	}
}
