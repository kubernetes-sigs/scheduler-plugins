// postfilter_plugin.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	_ *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	if ap := pl.getActivePlan(); ap != nil && !ap.PlanDoc.Completed {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: active plan in progress")
	}
	_ = pl.pruneStaleSetEntries(pl.Blocked)

	// Batch on PostFilter?
	if optimizeInBatches() && optimizeAtPostFilter() {
		klog.V(2).InfoS("PostFilter: batched pod", "pod", klog.KObj(pending))
		pl.Batched.AddPod(pending)
		return nil, framework.NewStatus(framework.Pending, "PostFilter: batched pod")
	}

	// Every-preemptor flow (single)
	if optimizeForEvery() && optimizeAtPostFilter() {
		klog.InfoS("PostFilter: start", "pod", klog.KObj(pending))
		ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
		defer cancel()

		startTime := time.Now()
		out, err := pl.solve(ctxSolve, SolveSingle, pending, nil, SolverTimeout)
		solverDuration := time.Since(startTime)
		if err != nil || out.NominatedNode == "" {
			klog.ErrorS(err, "PostFilter: solver found no solution", "pod", klog.KObj(pending), "duration", time.Since(startTime))
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: solver found no solution")
		}

		plan, ap, err := pl.registerPlan(ctx, out, pending)
		if err != nil {
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
		}

		// Skip plan execution if no moves or evictions
		if len(plan.Moves) > 0 || len(plan.Evicts) > 0 {
			if err := pl.executePlan(ctx, plan); err != nil {
				klog.ErrorS(err, "PostFilter: plan execution failed")
				pl.onPlanSettled()
				return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: plan execution failed")
			}
		}

		klog.InfoS("PostFilter: plan execution finished",
			"solverStatus", out.Status,
			"pod", klog.KObj(pending),
			"node", out.NominatedNode,
			"planID", ap.ID,
			"moved", len(plan.Moves),
			"evicted", len(plan.Evicts),
			"postFilterDuration", time.Since(startTime),
			"solverDuration", solverDuration,
		)

		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{
				NominatedNodeName: out.NominatedNode,
				NominatingMode:    framework.ModeOverride,
			},
		}, framework.NewStatus(framework.Success, "PostFilter: nominated after plan execution")
	}

	return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: no cross-node strategy enabled")
}
