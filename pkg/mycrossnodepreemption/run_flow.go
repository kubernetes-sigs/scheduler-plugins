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
	label := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(label + ": another plan active; skipping")
			return "", ErrActiveInProgress
		}
	}

	start := time.Now()

	// Optimize-specific setup
	var (
		solveMode   SolveMode
		preemptor   *v1.Pod
		batchedPods []*v1.Pod
	)
	if optimizeContinuous() { // Continuous
		solveMode = SolveContinuous
	} else if optimizeBatch() { // Batch
		solveMode = SolveBatch
		_ = pl.pruneSet(pl.Batched, "Batched")
		batchedPods = pl.snapshotBatch()
		if len(batchedPods) == 0 {
			klog.InfoS(label + ": no batched pod(s) to schedule")
			pl.leaveActive()
			return "", ErrNoop
		}
	} else { // Every
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// Fetch nodes and pods ONCE for this flow
	nodes, err := pl.getNodes()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, label+": failed to list nodes")
		return "", err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, label+": failed to list pods")
		return "", err
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(solveMode, nodes, pods, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, label+": failed to build solver input")
		pl.leaveActive()
		return "", err
	}
	bestSolver, anyFeasible := pl.runSolvers(ctx, solverInput, nodes, pods)
	// Check if all solvers are infeasible -> ErrNoOptimalOrFeasible
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, label+": no optimal/feasible solution from any solver")
		return "", ErrNoOptimalOrFeasible
	}

	// Take Active late for Continuous (only now that we know it's worth applying a plan).
	if optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.InfoS("Continuous: another plan active; skipping")
			return "", ErrActiveInProgress
		}
	}

	// Count new and total pods. If no pending pods to be scheduled -> ErrNoop
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestSolver.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(label + ": no pending pod(s) to be scheduled; skipping")
		pl.leaveActive()
		pl.onPlanSettled(PlanStatusFailed)
		return "", ErrNoop
	}

	// Register and execute storedPlan
	storedPlan, ap, targetNode, err := pl.registerPlan(ctx, bestSolver, preemptor, pods)
	if err != nil {
		// keep single-preemptor blocked on error
		if solveMode == SolveSingle && preemptor != nil {
			pl.Blocked.AddPod(preemptor)
		}
		klog.ErrorS(err, label+": register plan failed")
		pl.leaveActive()
		return "", ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if len(storedPlan.Plan.Moves) > 0 || len(storedPlan.Plan.Evicts) > 0 {
		if err := pl.executePlan(storedPlan); err != nil {
			klog.ErrorS(err, "Plan execution failed")
			pl.onPlanSettled(PlanStatusFailed)
		}
	}

	// If in Batch mode activate batched pods, now that the plan is in place.
	if optimizeBatch() {
		pl.activateBatchedPods(batchedPods, 0)
	}

	// Build and return result
	bestSolverSummary := summarizeAttempt(bestSolver)
	klog.InfoS(label+": plan execution finished; waiting for settlement",
		"planID", ap.ID,
		"bestSolver", bestSolverSummary,
		"nominated", targetNode,
		"batchSize", len(batchedPods),
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	return targetNode, nil
}
