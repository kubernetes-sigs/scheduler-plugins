// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind is called after a pod is bound to a node.
// It is used to check if the active scheduling plan is still in progress.
func (pl *MyCrossNodePreemption) PostBind(ctx context.Context, _ *framework.CycleState, _ *v1.Pod, _ string) {
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
		pl.markPlanCompleted(ctx, planID) // idempotent
	}
}
