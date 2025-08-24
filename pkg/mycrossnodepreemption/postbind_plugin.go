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
	ap := pl.getActive()
	if ap == nil || ap.PlanDoc.Completed {
		return
	}
	ok, err := pl.isPlanCompleted(ctx, ap.PlanDoc)
	if err != nil {
		pl.onPlanSettled()
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}
	if ok {
		if pl.onPlanSettled() {
			pl.markPlanCompleted(ctx, ap.ID) // idempotent
		}
	}
}
