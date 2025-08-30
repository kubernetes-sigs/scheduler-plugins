// postfilter_plugin.go (refactored)
package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	_ *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {

	if pl.Active.Load() {
		pl.Blocked.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: active plan in progress")
	}

	_ = pl.pruneStaleSetEntries(pl.Blocked)

	switch pl.decideStrategy(PhasePostFilter) {
	case DecidePassThrough:
		// pass-through => unschedulable here:
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: no cross-node strategy enabled")

	case DecideBatch:
		klog.V(V2).InfoS("PostFilter: batched pod", "pod", klog.KObj(pending))
		pl.Batched.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: batched pod")

	case DecideBlockActive:
		pl.Blocked.AddPod(pending)
		return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: active plan in progress")

	case DecideEvery:
		klog.InfoS("PostFilter: start", "pod", klog.KObj(pending))
		res, err := pl.runSingleFlow(ctx, pending, PhasePostFilter)
		if err != nil {
			if err == ErrActiveInProgress {
				pl.Blocked.AddPod(pending)
				return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: active plan in progress")
			}
			if err == ErrNoNomination {
				return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: solver found no solution")
			}
			klog.ErrorS(err, "PostFilter: plan failed", "pod", klog.KObj(pending))
			return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: plan failed")
		}

		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{
				NominatedNodeName: res.nominated,
				NominatingMode:    framework.ModeOverride,
			},
		}, framework.NewStatus(framework.Success, "PostFilter: nominated after plan execution")

	default:
		klog.Error("PostFilter: unexpected decision")
		return nil, framework.NewStatus(framework.Error, "PostFilter: unexpected decision")
	}

	// unreachable
}
