// optimization_flow.go

package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runOptimizationFlow runs the optimisation flow for the given phase (AllSynch,
// AllAsynch, Single). For Single phase, the preemptor must be provided.
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *SharedState) runOptimizationFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
	strategy := combinedModeToString()

	// Periodic-sync/Per-pod: take Active early.
	// Async modes: take Active later.
	if !isAsyncSolving() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	start := time.Now()

	// Plan context: snapshot, solver input, baseline, pending count.
	nodes, pods, inp, baselineScore, pendingPrePlan, err := pl.planContext(preemptor)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanPreparationFailed), "err", err)
		pl.leaveActive()
		return nil, nil, "", nil, nil, err
	}

	// Nothing to do
	if pendingPrePlan == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPods))
		pl.leaveActive()
		return nil, baselineScore, "baseline", nil, nil, ErrNoPendingPods
	}

	klog.InfoS(
		msg(strategy, "starting solvers"),
		"pending", pendingPrePlan,
		"totalPods", len(pods),
		"nodes", len(nodes),
	)

	// Plan computation
	bestName, hadImproving, bestAttempt, attempts := pl.planComputation(ctx, inp, nodes, pods, baselineScore)

	// Check if anything was feasible and improving
	if !hadImproving {
		klog.Error(msg(strategy, InfoNoImprovingSolutionFromAnySolver))
		pl.leaveActive()
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrNoImprovingSolutionFromAnySolver.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrNoImprovingSolutionFromAnySolver
	}

	// Async modes: take Active now that we know it is worth applying the plan.
	if isAsyncSolving() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrActiveInProgress.Error())
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	// How much is actually schedulable?
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestAttempt.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsToSchedule))
		pl.leaveActive()
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrNoPendingPodsToSchedule.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrNoPendingPodsToSchedule
	}

	// Plan registration
	plan, ap, err := pl.planRegistration(ctx, *bestAttempt, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanRegistrationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		pl.exportSolverStatsToConfigMap(
			context.Background(), strategy, baselineScore, bestName, attempts,
			ErrPlanRegistration.Error(),
		)
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrPlanRegistration
	}

	// Plan eviction and recreate standalone pods
	if err := pl.planActivation(plan, pods); err != nil {
		klog.Error(msg(strategy, InfoPlanActivationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrPlanActivationFailed.Error())
		return nil, baselineScore, bestName, bestAttempt, attempts, ErrPlanActivationFailed
	}

	// Start a periodically plan completion watcher. The watcher stops itself.
	pl.startPlanCompletionWatch(ap)

	// Export stats (success)
	pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, "")

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
