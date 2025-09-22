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

	phaseLabel := "PostBind"

	// Early exit if system pods or if no active plan
	ap := pl.getActivePlan()
	if pod.Namespace == SystemNamespace || ap == nil {
		return
	}

	// Check if the pod is relevant to the active plan
	relevant := false
	if _, ok := ap.PlacementByName[combineNsName(pod.Namespace, pod.Name)]; ok {
		relevant = true
	} else if wk, ok := topWorkload(pod); ok {
		_, relevant = ap.WorkloadPerNodeCnts[wk.String()]
	}
	if !relevant {
		klog.V(MyV).InfoS(phaseLabel+": irrelevant", "pod", klog.KObj(pod))
		return
	}

	// Check if the plan is completed
	ok, err := pl.isPlanCompleted(ctx, ap, pod)
	if err != nil {
		klog.V(MyV).ErrorS(err, phaseLabel+": "+InfoPlanCompletionFailed)
		return
	}
	if !ok {
		klog.V(MyV).InfoS(phaseLabel+": "+InfoActivePlanInProgress, "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}
	// Complete the plan
	if pl.onPlanSettled(PlanStatusCompleted) {
		klog.InfoS(phaseLabel+": "+InfoPlanCompleted, "planID", ap.ID)
	}
}
