// postfilter_plugin.go (refactored)
package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostFilter is called after filtering a pod.
// It is a replacement for the default preemption with cross-node preemption logic implemented.
// It catch all pods not handled by the default scheduling.
func (pl *MyCrossNodePreemption) PostFilter(ctx context.Context, _ *framework.CycleState, pending *v1.Pod, _ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	if pl.IsActivePlan() {
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
		res, err := pl.runFlow(ctx, PhasePostFilter, pending)
		if err != nil {
			if err == ErrActiveInProgress {
				pl.Blocked.AddPod(pending)
				return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: active plan in progress")
			}
			if err == ErrSolver {
				return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: solver failed")
			}
			// Else ErrRegisterPlan
			return nil, framework.NewStatus(framework.Unschedulable, "PostFilter: register plan failed")
		}

		// Return the result with the nominated node information which the scheduler will use to bind the pod.
		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{
				NominatedNodeName: res.Nominated,
				NominatingMode:    framework.ModeOverride,
			},
		}, framework.NewStatus(framework.Success, "PostFilter: nominated after plan execution")

	default:
		klog.Error("PostFilter: unexpected decision")
		return nil, framework.NewStatus(framework.Error, "PostFilter: unexpected decision")
	}
}
