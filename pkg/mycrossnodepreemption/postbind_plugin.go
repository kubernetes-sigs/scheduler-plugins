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
// postbind_plugin.go
func (pl *MyCrossNodePreemption) PostBind(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, _ string) {
	if pod.Namespace == "kube-system" {
		return
	}
	if !pl.IsActivePlan() {
		return
	}
	// Quickly bail if this pod isn't part of the current plan.
	if !pl.allowedByActivePlan(pod) {
		klog.V(V2).InfoS("PostBind: irrelevant", "pod", klog.KObj(pod))
		return
	}

	// Snapshot the plan atomically for this check.
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc == nil {
		return
	}
	ok, err := pl.isPlanCompleted(ctx, ap, pod)
	if err != nil {
		_ = pl.onPlanSettled(PlanStatusFailed)
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(V2).InfoS("PostBind: still in progress", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}

	// Only settle if the same plan is still active.
	if cur := pl.getActivePlan(); cur != nil && cur.ID == ap.ID {
		pl.onPlanSettled(PlanStatusCompleted)
	}
}
