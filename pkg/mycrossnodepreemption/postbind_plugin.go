// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind runs after a pod is bound by the scheduler.
// We check if the plan is completed.
func (pl *MyCrossNodePreemption) PostBind(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeName string,
) {
	sp, cmName := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return
	}

	if ok, err := pl.isPlanCompleted(ctx, sp); err != nil {
		klog.ErrorS(err, "PostBind: completion check failed")
	} else if ok {
		pl.clearActivePlan()
		pl.markPlanCompleted(ctx, cmName)
		klog.InfoS("PostBind: plan completed")
	} else {
		klog.V(2).InfoS("PostBind: plan still active")
	}
}
