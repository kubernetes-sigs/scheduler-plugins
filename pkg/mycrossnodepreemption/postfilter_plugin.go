// postfilter_plugin.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostFilter is called at the end of scheduling cycle if a pod does not succeed to be scheduled "normally" (without preemption).
// It is used, here, to try schedule the pod using our plugin, by doing cross-node preemption.
func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	// A plan is running → don't start (or grow) cohorts here; let PreEnqueue catch newcomers.
	if sp, _ := pl.getActivePlan(); sp != nil && !sp.Completed {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: active plan in progress")
	}
	_ = pl.pruneBlockedStale()

	// Batching strategy via PostFilter: only unschedulable pods get batched.
	if batchAtPostFilter() {
		klog.V(2).InfoS("PostFilter: batched pod", "pod", klog.KObj(pending))
		pl.Batched.AddPod(pending)
		return nil, framework.NewStatus(framework.Pending, "PostFilter: batched pod")
	}

	// Every-preemptor strategy
	if ModeEveryPreemptor {
		klog.InfoS("PostFilter: start", "pod", klog.KObj(pending))
		ctxSolve, cancel := context.WithTimeout(ctx, PythonSolverTimeout)
		defer cancel()
		startTime := time.Now()
		out, err := pl.runPythonOptimizerSingle(ctxSolve, pending, PythonSolverTimeout)
		okStatus := out != nil && (out.Status == "OPTIMAL" || out.Status == "FEASIBLE")
		if err != nil || !okStatus || out.NominatedNode == "" {
			if err != nil {
				klog.ErrorS(err, "PostFilter: solver failed")
			}
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: solver failed")
		}

		plan, err := pl.translatePlanFromSolver(out, pending)
		if err != nil {
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: plan translation failed")
		}
		plan.TargetNode = out.NominatedNode

		if out != nil {
			klog.InfoS("PostFilter: solver finished", "status", out.Status, "duration", time.Since(startTime))
		} else {
			klog.InfoS("PostFilter: solver finished", "status", "nil", "duration", time.Since(startTime))
		}

		var planID string

		if cmName, err := pl.exportPlanToConfigMap(context.Background(), plan, out, pending); err == nil {
			lite, byName, rsDesired, _ := pl.materializePlanDocs(plan, out, pending)
			pl.setActivePlan(&StoredPlan{
				Completed:        false,
				GeneratedAt:      time.Now().UTC(),
				PluginVersion:    Version,
				PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
				PendingUID:       string(pending.UID),
				TargetNode:       plan.TargetNode,
				SolverOutput:     out,
				Plan:             lite,
				PlacementsByName: byName,
				WkDesiredPerNode: rsDesired,
			}, cmName)
			_, planID = pl.getActivePlan()
			pl.startPlanTimeout(planID, PlanExecutionTTL)
		}

		if err := pl.executePlan(ctx, plan); err != nil {
			klog.ErrorS(err, "PostFilter: plan execution failed")
			pl.onPlanSettled()
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: plan execution failed")
		}

		klog.InfoS("PostFilter: plan execution finished",
			"pod", klog.KObj(pending),
			"node", out.NominatedNode,
			"planID", planID,
			"moved", len(plan.PodMovements),
			"evicted", len(plan.VictimsToEvict),
			"unscheduled", pl.countUnscheduledPods())

		return &framework.PostFilterResult{
			NominatingInfo: &framework.NominatingInfo{
				NominatedNodeName: out.NominatedNode,
				NominatingMode:    framework.ModeOverride,
			},
		}, framework.NewStatus(framework.Success, "PostFilter: nominated after plan execution")
	}

	// Neither batching nor every-preemptor enabled (shouldn't happen due to New() validation).
	return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PostFilter: no cross-node strategy enabled")
}
