// postbind_hook.go

package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind is called after a pod is bound to a node.
// It is used to check if the active scheduling plan is still in progress.
func (pl *SharedState) PostBind(ctx context.Context, _ *framework.CycleState, pending *v1.Pod, _ string) {

	// stage := "PostBind"

	// // Early exit if system pods or if no active plan
	// ap := pl.getActivePlan()
	// if pending.Namespace == SystemNamespace || ap == nil {
	// 	return
	// }

	// // Check if the pod is relevant to the active plan
	// relevant := false
	// if _, ok := ap.PlacementByName[combineNsName(pending.Namespace, pending.Name)]; ok {
	// 	relevant = true
	// } else if wk, ok := topWorkload(pending); ok {
	// 	_, relevant = ap.WorkloadPerNodeCnts[wk.String()]
	// }
	// if !relevant {
	// 	klog.V(MyV).InfoS(msg(stage, "irrelevant"), "pod", klog.KObj(pending))
	// 	return
	// }

	// // Check if the plan is completed
	// ok, err := pl.isPlanCompleted(ctx, ap, pending)
	// if err != nil {
	// 	klog.V(MyV).ErrorS(err, msg(stage, InfoPlanCompletionFailed))
	// 	return
	// }
	// if !ok {
	// 	klog.V(MyV).InfoS(msg(stage, InfoActivePlanInProgress), "planID", ap.ID, "pod", klog.KObj(pending))
	// 	return
	// }
	// // Complete the plan
	// if pl.onPlanSettled(PlanStatusCompleted) {
	// 	klog.InfoS(msg(stage, InfoPlanCompleted), "planID", ap.ID)
	// }
}
