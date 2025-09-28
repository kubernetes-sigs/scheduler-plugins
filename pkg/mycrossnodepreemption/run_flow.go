// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the flow for the given phase (AllSynch, AllAsynch, Single).
// For Single phase, the preemptor must be provided.
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, *SolverResult, error) {

	strategy := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(msg(strategy, InfoActivePlanInProgress))
			pl.exportSolverStatsConfigMap(ctx, strategy, nil, ErrActiveInProgress.Error(), &SolverResult{Name: "baseline"}, nil)
			return nil, nil, ErrActiveInProgress
		}
	}

	start := time.Now()
	baselineResult := SolverResult{Name: "baseline"}

	// Fetch nodes and pods ONCE for this flow
	nodes, err := pl.getNodes()
	if err != nil {
		klog.Error(msg(strategy, "failed to list nodes"))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, nil, err.Error(), &baselineResult, nil)
		return nil, nil, err
	}
	pods, err := pl.getPods()
	if err != nil {
		klog.Error(msg(strategy, "failed to list pods"))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, nil, err.Error(), &baselineResult, nil)
		return nil, nil, err
	}

	// Count pending pods
	pendingPostPlan := countPendingPods(pods)
	if pendingPostPlan == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPods))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, nil, ErrNoPendingPods.Error(), &baselineResult, nil)
		return nil, nil, ErrNoPendingPods
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(nodes, pods, preemptor)
	if err != nil {
		klog.Error(msg(strategy, "failed to build solver input"))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, nil, ErrNoPendingPods.Error(), &baselineResult, nil)
		return nil, nil, err
	}

	bestSolver, hadImproving, attempts, baselineScore := pl.runSolvers(ctx, solverInput, nodes, pods)

	// Check if all solvers are infeasible
	if !hadImproving {
		klog.Error(msg(strategy, InfoNoImprovingSolutionFromAnySolver))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, nil, ErrNoImprovingSolutionFromAnySolver.Error(), &SolverResult{Name: "baseline"}, nil)
		return nil, &bestSolver, ErrNoImprovingSolutionFromAnySolver
	}

	// Take Active late for AllSynch (only now that we know it's worth applying a plan).
	if optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			pl.exportSolverStatsConfigMap(ctx, strategy, &baselineScore, ErrActiveInProgress.Error(), &bestSolver, attempts)
			return nil, nil, ErrActiveInProgress
		}
	}

	// Count new and total pods and return if no pending pods is to be scheduled
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestSolver.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsToSchedule))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, &baselineScore, ErrNoPendingPodsToSchedule.Error(), &bestSolver, attempts)
		return nil, &bestSolver, ErrNoPendingPodsToSchedule
	}

	// Register and execute storedPlan
	plan, ap, err := pl.registerPlan(ctx, bestSolver, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoRegisterPlanFailed))
		pl.onPlanSettled(PlanStatusFailed)
		pl.exportSolverStatsConfigMap(ctx, strategy, &baselineScore, ErrRegisterPlan.Error(), &bestSolver, attempts)
		return nil, &bestSolver, ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if err := pl.executePlan(plan); err != nil {
		klog.Error(msg(strategy, InfoPlanExecutionFailed))
		pl.onPlanSettled(PlanStatusFailed)
		pl.exportSolverStatsConfigMap(ctx, strategy, &baselineScore, ErrPlanExecutionFailed.Error(), &bestSolver, attempts)
		return nil, &bestSolver, ErrPlanExecutionFailed
	}

	// If in all modes activate planned pending pods (now that the plan is in place).
	if optimizeAllSynch() || optimizeAllAsynch() || optimizeManualAllSynch() {
		pl.activatePlannedPending(plan, pods)
	}

	pl.exportSolverStatsConfigMap(ctx, strategy, &baselineScore, "", &bestSolver, attempts)

	// Build and return result
	bestSolverSummary := summarizeAttempt(bestSolver)
	klog.InfoS(msg(strategy, InfoPlanExecutionFinished),
		"planID", ap.ID,
		"bestSolver", bestSolverSummary,
		"pendingPostPlan", pendingPostPlan,
		"pendingScheduled", pendingScheduled,
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	// Return the stored plan for inspection (if needed)
	return plan, &bestSolver, nil
}
