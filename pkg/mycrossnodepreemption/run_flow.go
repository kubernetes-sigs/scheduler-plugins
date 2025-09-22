// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the flow for the given phase (Continuous, Batch, Single).
// For Single phase, the singlePod must be provided (the preemptor).
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, singlePod *v1.Pod) (targetNode string, err error) {
	strategy := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(strategy + ": " + InfoActivePlanInProgress + "; skipping")
			return "", ErrActiveInProgress
		}
	}

	start := time.Now()

	// Fetch nodes and pods ONCE for this flow
	nodes, err := pl.getNodes()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, strategy+": failed to list nodes")
		return "", err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, strategy+": failed to list pods")
		return "", err
	}

	// Count pending pods
	pendingCount := countPendingPods(pods)

	// Optimize-specific setup
	var (
		solveMode SolveMode
		preemptor *v1.Pod
	)
	if optimizeAllAsynch() || optimizeAllSynch() {
		solveMode = SolveAll
		if pendingCount == 0 {
			klog.InfoS(strategy + ": " + InfoNoPendingPods)
			pl.leaveActive()
			return "", ErrNoop
		}
	} else { // Every
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(solveMode, nodes, pods, preemptor)
	if err != nil {
		klog.ErrorS(err, strategy+": failed to build solver input")
		pl.leaveActive()
		return "", err
	}
	bestSolver, anyFeasible := pl.runSolvers(ctx, solverInput, nodes, pods)
	// Check if all solvers are infeasible -> ErrNoOptimalOrFeasible
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, strategy+": "+InfoNoOptimalOrFeasible)
		return "", ErrNoOptimalOrFeasible
	}

	// Take Active late for Continuous (only now that we know it's worth applying a plan).
	if optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.InfoS("Continuous: " + InfoActivePlanInProgress + "; skipping")
			return "", ErrActiveInProgress
		}
	}

	// Count new and total pods. If no pending pods to be scheduled -> ErrNoop
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestSolver.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(strategy + ": " + InfoNoPendingPodsToSchedule + "; skipping")
		pl.leaveActive()
		pl.onPlanSettled(PlanStatusFailed)
		return "", ErrNoop
	}

	// Register and execute storedPlan
	plan, ap, targetNode, err := pl.registerPlan(ctx, bestSolver, preemptor, pods)
	if err != nil {
		// keep single-preemptor blocked on error
		if solveMode == SolveSingle && preemptor != nil {
			pl.BlockedWhileActive.AddPod(preemptor)
		}
		klog.ErrorS(err, strategy+": "+InfoRegisterPlanFailed)
		pl.leaveActive()
		return "", ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if err := pl.executePlan(plan); err != nil {
		klog.ErrorS(err, strategy+": "+InfoPlanExecutionFailed)
		pl.onPlanSettled(PlanStatusFailed)
	}

	// If in all modes activate planned pending pods (now that the plan is in place).
	if optimizeAllSynch() || optimizeAllAsynch() {
		pl.activatePlannedPending(plan, pods)
	}

	// Build and return result
	bestSolverSummary := summarizeAttempt(bestSolver)
	klog.InfoS(strategy+": plan execution finished; waiting for settlement",
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
