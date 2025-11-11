// run_flow.go

package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the flow for the given phase (AllSynch, AllAsynch, Single).
// For Single phase, the preemptor must be provided.
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *MyPriorityOptimizer) runFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
	strategy := strategyToString()

	// Batch/Single: take Active early.
	// Continuous: take Active later (only if we can worth apply the plan after solving).
	if !optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(msg(strategy, InfoActivePlanInProgress))
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	start := time.Now()

	// Fetch cluster view once
	nodes, err := pl.getNodes()
	if err != nil {
		klog.Error(msg(strategy, "failed to list nodes"))
		pl.leaveActive()
		return nil, nil, "", nil, nil, err
	}
	pods, err := pl.getPods()
	if err != nil {
		klog.Error(msg(strategy, "failed to list pods"))
		pl.leaveActive()
		return nil, nil, "", nil, nil, err
	}

	// Build input and run solvers
	inp, err := pl.buildSolverInput(nodes, pods, preemptor)
	if err != nil {
		klog.Error(msg(strategy, "failed to build solver input"))
		pl.leaveActive()
		return nil, nil, "", nil, nil, err
	}
	// Compute baseline score
	baselineScore := buildBaselineScore(inp)

	// Nothing to do
	pendingPrePlan := countPendingPods(pods)
	if pendingPrePlan == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPods))
		pl.leaveActive()
		return nil, baselineScore, "baseline", nil, nil, ErrNoPendingPods
	}

	klog.InfoS(msg(strategy, "starting solvers"), "pending", pendingPrePlan, "totalPods", len(pods), "nodes", len(nodes))
	bestName, hadImproving, bestAttempt, attempts := pl.runSolvers(ctx, inp, nodes, pods, baselineScore)

	// Check if anything was feasible and improving
	if !hadImproving {
		klog.Error(msg(strategy, InfoNoImprovingSolutionFromAnySolver))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, ErrNoImprovingSolutionFromAnySolver.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrNoImprovingSolutionFromAnySolver
	}

	// Continuous: take Active now that we know it’s worth applying.
	if optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, ErrActiveInProgress.Error())
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	// How much is actually schedulable?
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestAttempt.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsToSchedule))
		pl.leaveActive()
		pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, ErrNoPendingPodsToSchedule.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrNoPendingPodsToSchedule
	}

	// Register and execute plan
	plan, ap, err := pl.registerPlan(ctx, *bestAttempt, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoRegisterPlanFailed))
		pl.onPlanSettled(PlanStatusFailed)
		pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, ErrRegisterPlan.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrRegisterPlan
	}
	if err := pl.executePlan(plan); err != nil {
		klog.Error(msg(strategy, InfoPlanExecutionFailed))
		pl.onPlanSettled(PlanStatusFailed)
		pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, ErrPlanExecutionFailed.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrPlanExecutionFailed
	}

	// Activate planned pending (if applicable)
	if optimizeAllSynch() || optimizeAllAsynch() || optimizeManualAllSynch() {
		pl.activatePlannedPending(plan, pods)
	}

	// Export stats (success)
	pl.exportSolverStatsConfigMap(ctx, strategy, baselineScore, bestName, attempts, "")

	// Log summary
	bestSummary := summarizeAttempt(*bestAttempt)
	klog.InfoS(
		msg(strategy, InfoPlanExecutionFinished),
		"planID", ap.ID,
		"bestAttempt", bestSummary,
		"pendingPrePlan", pendingPrePlan,
		"pendingScheduled", pendingScheduled,
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	return plan, baselineScore, bestName, bestAttempt, attempts, nil
}
