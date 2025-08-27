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
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc.Completed {
		klog.V(V2).InfoS("PostBind: no active plan; no completion check", "pod", klog.KObj(pod))
		return
	}

	// Only react for pods belonging to the active plan
	relevant := func() bool {
		if string(pod.UID) == ap.PlanDoc.PendingUID {
			return true
		}
		if _, ok := ap.PlanDoc.PlacementsByName[pod.Namespace+"/"+pod.Name]; ok {
			return true
		}
		if wk, ok := topWorkload(pod); ok {
			_, in := ap.PlanDoc.WkDesiredPerNode[wk.String()]
			return in
		}
		return false
	}()

	if !relevant {
		klog.InfoS("PostBind: irrelevant pod", "pod", klog.KObj(pod))
		return
	}

	ok, err := pl.isPlanCompleted(ctx, ap.PlanDoc, pod)

	if err != nil {
		_ = pl.onPlanSettled()
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}

	if !ok { // Plan is not completed
		klog.V(V2).InfoS("PostBind: plan still in progress", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}

	// Double-check we still act on the same plan after we have done the completion check; another PostBind may have taken over
	cur := pl.getActivePlan()
	if cur == nil || cur.ID != ap.ID {
		klog.V(V2).InfoS("PostBind: plan already settled", "planID", ap.ID, "pod", klog.KObj(pod))
		return
	}
	if pl.onPlanSettled() {
		pl.markPlanCompleted(ctx, ap.ID) // idempotent
	}
}
