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
func (pl *SharedState) runOptimizationFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, *PlannerScore, string, *PlannerResult, []PlannerResult, error) {
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
	nodes, pods, pendingPrePlan, inp, err := pl.planContext(preemptor)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanContextFailed), "err", err)
		pl.leaveActive()
		return nil, nil, "", nil, nil, err
	}
	baselineScore := inp.BaselineScore

	// Plan computation
	bestName, hadImp, bestAttempt, bestOut, attempts := pl.planComputation(ctx, inp)

	// Check if any solver solution was improving, if not, exit early.
	if !hadImp {
		klog.Error(msg(strategy, InfoNoImprovingSolutionFromAnySolver))
		pl.leaveActive()
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrNoImprovingSolutionFromAnySolver.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrNoImprovingSolutionFromAnySolver
	}

	// Verify that plan (still) can be applied
	// Mainly for async modes, where the cluster state may have changed since plan computation.
	ok, why := pl.isPlanApplicable(bestOut, nodes, pods)
	if !ok {
		klog.Error(msg(strategy, InfoPlanNotApplicable), "solver", bestName, "status", bestOut.Status, "reason", why)
		pl.leaveActive()
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrPlanNotApplicable.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanNotApplicable
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
	pendingScheduled, totalPrePlan, totalPostPlan := computePlanPodCounts(bestOut, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsScheduled))
		pl.leaveActive()
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrNoPendingPodsScheduled.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrNoPendingPodsScheduled
	}

	// NOTE: If any error occurs from here on, we must call onPlanCompleted instead of just leaveActive

	// Plan registration
	plan, ap, err := pl.planRegistration(ctx, *bestAttempt, bestOut, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanRegistrationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		pl.exportSolverStatsToConfigMap(
			context.Background(), strategy, baselineScore, bestName, attempts,
			ErrPlanRegistration.Error(),
		)
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanRegistration
	}

	// Plan eviction and recreate standalone pods
	if err := pl.planActivation(plan, pods); err != nil {
		klog.Error(msg(strategy, InfoPlanActivationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, ErrPlanActivationFailed.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanActivationFailed
	}

	// Start a periodically plan completion watcher. The watcher stops itself.
	pl.startPlanCompletionWatch(ap)

	// Export stats (success)
	pl.exportSolverStatsToConfigMap(context.Background(), strategy, baselineScore, bestName, attempts, "")

	// Log summary
	klog.InfoS(
		msg(strategy, InfoPlanExecutionFinished),
		"planID", ap.ID,
		"bestAttempt", bestAttempt,
		"pendingPrePlan", pendingPrePlan,
		"pendingScheduled", pendingScheduled,
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	return plan, &baselineScore, bestName, bestAttempt, attempts, nil
}
