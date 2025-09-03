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
func (pl *MyCrossNodePreemption) PostBind(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, _ string) {
	// Don't check kube-system pods
	if pod.Namespace == "kube-system" {
		return
	}
	if !pl.IsActivePlan() {
		return
	}
	ap := pl.getActivePlan()
	if !pl.allowedByActivePlan(pod) {
		klog.V(V2).InfoS("PostBind: irrelevant", "pod", klog.KObj(pod))
		return
	}
	ok, err := pl.isPlanCompleted(ctx, ap.PlanDoc, pod)
	if err != nil {
		_ = pl.onPlanSettled(PlanStatusFailed)
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(V2).InfoS("PostBind: still in progress", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}
	// Double-check still same plan
	cur := pl.getActivePlan()
	if cur != nil && cur.ID == ap.ID {
		pl.onPlanSettled(PlanStatusCompleted)
	}
}
