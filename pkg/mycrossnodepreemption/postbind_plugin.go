// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// TODO: Reach to here in this file...

// PostBind is called after a pod is bound to a node.
// It is used to check if the active scheduling plan is still in progress.
// postbind_plugin.go
func (pl *MyCrossNodePreemption) PostBind(ctx context.Context, _ *framework.CycleState, pod *v1.Pod, _ string) {
	ap := pl.getActivePlan()
	if pod.Namespace == "kube-system" || ap == nil {
		return
	}
	relevant := false
	if _, ok := ap.PlacementByName[combineNsName(pod.Namespace, pod.Name)]; ok {
		relevant = true
	} else if wk, ok := topWorkload(pod); ok {
		_, relevant = ap.WorkloadPerNodeCnts[wk.String()]
	}
	if !relevant {
		klog.V(MyVerbosity).InfoS("PostBind: irrelevant", "pod", klog.KObj(pod))
		return
	}

	ok, err := pl.isPlanCompleted(ctx, ap, pod)
	if err != nil {
		//_ = pl.onPlanSettled(PlanStatusFailed)
		klog.V(MyVerbosity).ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(MyVerbosity).InfoS("PostBind: still in progress", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}
	if pl.onPlanSettled(PlanStatusCompleted) {
		klog.InfoS("PostBind: plan completed", "planID", ap.ID)
	}
}
