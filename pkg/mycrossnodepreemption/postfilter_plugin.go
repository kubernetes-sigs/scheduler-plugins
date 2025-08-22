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

// ---------------------------- PostFilter ----------------------------

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {

	// If a plan is already running, don't interfere.
	if sp, _ := pl.getActivePlan(); sp != nil && !sp.Completed {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "active cross-node plan in progress")
	}

	// Batch mode: never run single-preemptor here.
	if BatchModeEnabled {
		pl.addToBatch(pending)
		return nil, framework.NewStatus(framework.Pending, "batched for cross-node optimization")
	}
	if !PostFilterSinglePreemptor {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "single-preemptor disabled")
	}

	klog.InfoS("PostFilter(single)", "pending", klog.KObj(pending),
		"cpu(m)", getPodCPURequest(pending), "mem(MiB)", bytesToMiB(getPodMemoryRequest(pending)))

	// --- Solve for this one preemptor (snapshot for this cycle). ---
	ctxSolve, cancel := context.WithTimeout(ctx, PythonSolverTimeout)
	defer cancel()
	startTime := time.Now()
	out, err := pl.runPythonOptimizerSingle(ctxSolve, pending, PythonSolverTimeout)
	klog.InfoS("PostFilter: solver finished", "duration", time.Since(startTime))
	if err != nil || out == nil || out.Status != "OK" || out.NominatedNode == "" {
		if err != nil {
			klog.ErrorS(err, "single-preemptor solve failed")
		}
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "no cross-node plan")
	}

	// Translate & execute (evictions/moves **for other pods** only).
	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil {
		klog.ErrorS(err, "translate plan failed")
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "plan translation failed")
	}
	plan.TargetNode = out.NominatedNode

	// Optional: persist the plan for observability.
	if cmName, err := pl.exportPlanToConfigMap(context.Background(), plan, out, pending); err == nil {
		lite, byName, rsDesired, _ := pl.materializePlanDocs(plan, out, pending)
		pl.setActivePlan(&StoredPlan{
			Completed:              false,
			GeneratedAt:            time.Now().UTC(),
			PluginVersion:          Version,
			PendingPod:             fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
			PendingUID:             string(pending.UID),
			TargetNode:             plan.TargetNode,
			SolverOutput:           out,
			Plan:                   lite,
			PlacementsByName:       byName,
			WorkloadDesiredPerNode: rsDesired,
		}, cmName)
		_, planID := pl.getActivePlan()
		pl.startPlanTimeout(planID, PlanExecutionTTL)
	}

	if err := pl.executePlan(ctx, plan); err != nil {
		klog.ErrorS(err, "plan execution failed")
		pl.onPlanSettled()
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "plan execution failed")
	}

	// Done preparing capacity. Nominate and let the scheduler retry the pod on that node.
	klog.InfoS("PostFilter: finished nominating", "pod", klog.KObj(pending), "node", out.NominatedNode, "movements", len(plan.PodMovements), "evictions", len(plan.VictimsToEvict))
	pl.onPlanSettled()

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{
			NominatedNodeName: out.NominatedNode,
			NominatingMode:    framework.ModeOverride,
		},
	}, framework.NewStatus(framework.Success, "nominated after cross-node plan")
}
