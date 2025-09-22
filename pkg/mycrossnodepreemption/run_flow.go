// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the flow for the given phase (AllSynch, AllAsynch, Single).
// For Single phase, the singlePod must be provided (the preemptor).
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, singlePod *v1.Pod) (targetNode string, err error) {

	strategy := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(msg(strategy, InfoActivePlanInProgress))
			return "", ErrActiveInProgress
		}
	}

	start := time.Now()

	// Fetch nodes and pods ONCE for this flow
	nodes, err := pl.getNodes()
	if err != nil {
		pl.leaveActive()
		klog.Error(msg(strategy, "failed to list nodes"))
		return "", err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.Error(msg(strategy, "failed to list pods"))
		return "", err
	}

	// Count pending pods
	pendingCount := countPendingPods(pods)
	if pendingCount == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPods))
		pl.leaveActive()
		return "", ErrNoop
	}

	// Optimize-specific setup
	var (
		solveMode SolveMode
		preemptor *v1.Pod
	)
	if optimizeAllAsynch() || optimizeAllSynch() {
		solveMode = SolveAll
	} else { // Every
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(solveMode, nodes, pods, preemptor)
	if err != nil {
		klog.Error(msg(strategy, "failed to build solver input"))
		pl.leaveActive()
		return "", err
	}
	bestSolver, anyFeasible := pl.runSolvers(ctx, solverInput, nodes, pods)
	// Check if all solvers are infeasible -> ErrNoOptimalOrFeasible
	if !anyFeasible {
		pl.leaveActive()
		klog.Error(msg(strategy, InfoNoSolverSolution))
		return "", ErrNoSolverSolution
	}

	// Take Active late for AllSynch (only now that we know it's worth applying a plan).
	if optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			return "", ErrActiveInProgress
		}
	}

	// Count new and total pods. If no pending pods to be scheduled -> ErrNoop
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestSolver.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsToSchedule))
		pl.leaveActive()
		return "", ErrNoop
	}

	// Register and execute storedPlan
	plan, ap, targetNode, err := pl.registerPlan(ctx, bestSolver, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoRegisterPlanFailed))
		pl.leaveActive()
		return "", ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if err := pl.executePlan(plan); err != nil {
		klog.Error(msg(strategy, InfoPlanExecutionFailed))
		pl.onPlanSettled(PlanStatusFailed)
	}

	// If in all modes activate planned pending pods (now that the plan is in place).
	if optimizeAllSynch() || optimizeAllAsynch() {
		pl.activatePlannedPending(plan, pods)
	}

	// Build and return result
	bestSolverSummary := summarizeAttempt(bestSolver)
	klog.InfoS(msg(strategy, InfoPlanExecutionFinished),
		"planID", ap.ID,
		"bestSolver", bestSolverSummary,
		"nominated", targetNode,
		"pending", pendingCount,
		"pendingScheduled", pendingScheduled,
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	return targetNode, nil
}
