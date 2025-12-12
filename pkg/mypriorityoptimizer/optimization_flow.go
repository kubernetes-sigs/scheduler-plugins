// optimization_flow.go

package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Test Hooks
var (
	isAsyncSolvingFn = isAsyncSolving
	planContextFn    = func(pl *SharedState, preemptor *v1.Pod) ([]*v1.Node, []*v1.Pod, SolverInput, error) {
		return pl.planContext(preemptor)
	}
	planComputationFn = func(pl *SharedState, ctx context.Context, in SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return pl.planComputation(ctx, in)
	}
	isSolutionApplicableFn = func(pl *SharedState, out *SolverOutput, nodes []*v1.Node, pods []*v1.Pod) (bool, string) {
		return pl.isSolutionApplicable(out, nodes, pods)
	}
	computePlanPodCountsFn = func(out *SolverOutput, pods []*v1.Pod) (int, int, int) {
		return computePlanPodCounts(out, pods)
	}
	planRegistrationFn = func(pl *SharedState, ctx context.Context, res SolverResult, out *SolverOutput, preemptor *v1.Pod, pods []*v1.Pod) (*Plan, *ActivePlan, error) {
		return pl.planRegistration(ctx, res, out, preemptor, pods)
	}
	planActivationFn = func(pl *SharedState, plan *Plan, pods []*v1.Pod) error {
		return pl.planActivation(plan, pods)
	}
	startPlanCompletionWatchFn = func(pl *SharedState, ap *ActivePlan) {
		pl.startPlanCompletionWatch(ap)
	}
	exportSolverStatsFn = func(pl *SharedState, strategy string, baseline SolverScore, bestName string, attempts []SolverResult, errMsg string) {
		pl.exportSolverStatsToConfigMap(context.Background(), strategy, baseline, bestName, attempts, errMsg)
	}
)

// runOptimizationFlow runs the optimisation flow for the given phase (AllSynch,
// AllAsynch, Single). For Single phase, the preemptor must be provided.
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *SharedState) runOptimizationFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, *SolverScore, string, *SolverResult, []SolverResult, error) {
	strategy := getModeCombinedAsString()

	// Ensure only one optimization flow at a time.
	if !pl.tryEnterOptimizationFlow() {
		klog.InfoS(msg(strategy, InfoOptimizationInProgress))
		return nil, nil, "", nil, nil, ErrOptimizationInProgress
	}
	defer pl.tryLeaveOptimizationFlow()

	// Periodic-sync/Per-pod: take PlanActive early.
	// Async modes: take PlanActive later.
	if !isAsyncSolvingFn() {
		if !pl.tryEnterActivePlan() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	start := time.Now()

	// Plan context: snapshot, solver input, baseline, pending count.
	nodes, pods, inp, err := planContextFn(pl, preemptor)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanContextFailed), "err", err)
		pl.tryLeaveActivePlan()
		return nil, nil, "", nil, nil, err
	}
	baselineScore := inp.BaselineScore
	
	// Only proceed if there are pending pods to schedule.
	pendingPrePlan := countPendingPods(pods)
	if pendingPrePlan == 0 {
		return nil, &baselineScore, "", nil, nil, ErrNoPendingPods
	}

	// Plan computation
	bestName, hadImp, bestAttempt, bestOut, attempts := planComputationFn(pl, ctx, inp)

	// Check if any solver solution was improving, if not, exit early.
	if !hadImp {
		klog.Error(msg(strategy, InfoNoImprovingSolutionFromAnySolver))
		pl.tryLeaveActivePlan()
		exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrNoImprovingSolutionFromAnySolver.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrNoImprovingSolutionFromAnySolver
	}

	// Verify that plan (still) can be applied
	// Mainly for async modes, where the cluster state may have changed since plan computation.
	ok, why := isSolutionApplicableFn(pl, bestOut, nodes, pods)
	if !ok {
		klog.Error(msg(strategy, InfoPlanNotApplicable), "solver", bestName, "status", bestOut.Status, "reason", why)
		pl.tryLeaveActivePlan()
		exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrPlanNotApplicable.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanNotApplicable
	}

	// Async modes: take PlanActive now that we know it is worth applying the plan.
	if isAsyncSolvingFn() {
		if !pl.tryEnterActivePlan() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrActiveInProgress.Error())
			return nil, nil, "", nil, nil, ErrActiveInProgress
		}
	}

	// How much is actually schedulable?
	pendingScheduled, totalPrePlan, totalPostPlan := computePlanPodCountsFn(bestOut, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsScheduled))
		pl.tryLeaveActivePlan()
		exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrNoPendingPodsScheduled.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrNoPendingPodsScheduled
	}

	// NOTE: If any error occurs from here on, we must call onPlanCompleted instead of just leaveActivePlan.

	// Plan registration
	plan, ap, err := planRegistrationFn(pl, ctx, *bestAttempt, bestOut, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoPlanRegistrationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrPlanRegistration.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanRegistration
	}

	// Plan eviction and recreate standalone pods
	if err := planActivationFn(pl, plan, pods); err != nil {
		klog.Error(msg(strategy, InfoPlanActivationFailed))
		pl.onPlanCompleted(PlanStatusFailed)
		exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, ErrPlanActivationFailed.Error())
		return nil, &baselineScore, bestName, bestAttempt, attempts, ErrPlanActivationFailed
	}

	// Start a periodically plan completion watcher. The watcher stops itself.
	startPlanCompletionWatchFn(pl, ap)

	// Export stats (success)
	exportSolverStatsFn(pl, strategy, baselineScore, bestName, attempts, "")

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
