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
		klog.V(V2).InfoS("PostBind: irrelevant", "pod", klog.KObj(pod))
		return
	}

	ok, err := pl.isPlanCompleted(ctx, ap, pod)
	if err != nil {
		//_ = pl.onPlanSettled(PlanStatusFailed) // TODO_HC: comment out as sometimes we get pod not found
		klog.V(V2).ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(V2).InfoS("PostBind: still in progress", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}
	if cur := pl.getActivePlan(); cur != nil && cur.ID == ap.ID {
		pl.onPlanSettled(PlanStatusCompleted)
	}
}
