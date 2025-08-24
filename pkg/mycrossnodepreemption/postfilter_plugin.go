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

	if ap := pl.getActive(); ap != nil && !ap.PlanDoc.Completed {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: active plan in progress")
	}
	_ = pl.pruneBlockedStale()

	// Batch on PostFilter?
	if batchAtPostFilter() {
		klog.V(2).InfoS("PostFilter: batched pod", "pod", klog.KObj(pending))
		pl.Batched.AddPod(pending)
		return nil, framework.NewStatus(framework.Pending, "PostFilter: batched pod")
	}

	// Every-preemptor flow (single)
	if ModeEveryPreemptor {
		klog.InfoS("PostFilter: start", "pod", klog.KObj(pending))
		ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
		defer cancel()

		startTime := time.Now()
		out, err := pl.solve(ctxSolve, SolveSingle, pending, nil, SolverTimeout)
		if err != nil || out.NominatedNode == "" {
			if err != nil {
				klog.ErrorS(err, "PostFilter: solver failed")
			}
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: solver failed")
		}
		klog.InfoS("PostFilter: solver finished", "status", out.Status, "duration", time.Since(startTime))

		plan, ap, err := pl.publishPlan(ctx, out, pending)
		if err != nil {
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
		}

		if err := pl.executePlan(ctx, plan); err != nil {
			klog.ErrorS(err, "PostFilter: plan execution failed")
			pl.onPlanSettled()
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: plan execution failed")
		}

		klog.InfoS("PostFilter: plan execution finished",
			"pod", klog.KObj(pending),
			"node", out.NominatedNode,
			"planID", ap.ID,
			"moved", len(plan.Moves),
			"evicted", len(plan.Evicts),
			"unscheduled", pl.countUnscheduledPods(),
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
