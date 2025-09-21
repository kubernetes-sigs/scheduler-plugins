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
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, singlePod *v1.Pod) (*FlowResult, error) {
	label := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(label + ": another plan active; skipping")
			return nil, ErrActiveInProgress
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
			return nil, ErrNoop
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
		return nil, err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, label+": failed to list pods")
		return nil, err
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(solveMode, nodes, pods, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, label+": failed to build solver input")
		pl.leaveActive()
		return nil, err
	}
	bestOut, anyFeasible, chosenSolver := pl.runSolvers(ctx, solverInput, nodes, pods)
	// No improvement -> ErrNoImprovement
	if bestOut == nil {
		pl.leaveActive()
		klog.InfoS(label + ": no solver improved over baseline; skipping")
		return nil, ErrNoImprovement
	}
	// Check if all solvers are infeasible -> ErrNoOptimalOrFeasible
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, label+": no optimal/feasible solution from any solver")
		return nil, ErrNoOptimalOrFeasible
	}

	// Take Active late for Continuous (only now that we know it's worth applying a plan).
	if optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.InfoS("Continuous: another plan active; skipping")
			return nil, ErrActiveInProgress
		}
	}

	// Count new and total pods. If no pending pods to be scheduled -> ErrNoop
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestOut, pods)
	if pendingScheduled == 0 {
		klog.InfoS(label + ": no pending pod(s) to be scheduled; skipping")
		pl.onPlanSettled(PlanStatusFailed)
		return nil, ErrNoop
	}

	// Register and execute plan
	plan, ap, targetNode, err := pl.registerPlan(ctx, bestOut, chosenSolver, preemptor, pods)
	if err != nil {
		// keep single-preemptor blocked on error
		if solveMode == SolveSingle && preemptor != nil {
			pl.Blocked.AddPod(preemptor)
		}
		klog.ErrorS(err, label+": register plan failed")
		pl.leaveActive()
		return nil, ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if len(plan.Moves) > 0 || len(plan.Evicts) > 0 {
		if err := pl.executePlan(plan); err != nil {
			klog.ErrorS(err, "Plan execution failed")
			pl.onPlanSettled(PlanStatusFailed)
		}
	}

	// If in Batch mode activate batched pods, now that the plan is in place.
	if optimizeBatch() {
		pl.activateBatchedPods(batchedPods, 0)
	}

	// Build and return result
	res := &FlowResult{
		PlanID:        ap.ID,
		TargetNode:    targetNode,
		BatchSize:     len(batchedPods),
		TotalPrePlan:  totalPrePlan,
		TotalPostPlan: totalPostPlan,
		ChosenSolver:  chosenSolver,
		TotalDuration: time.Since(start),
	}
	klog.InfoS(label+": plan execution finished; waiting for settlement",
		"planID", res.PlanID,
		"chosenSolver", res.ChosenSolver,
		"nominated", res.TargetNode,
		"batchSize", res.BatchSize,
		"totalPrePlan", res.TotalPrePlan,
		"totalPostPlan", res.TotalPostPlan,
		"totalDuration", res.TotalDuration,
	)

	return res, nil
}

// FlowResult represents the result of a scheduling flow.
type FlowResult struct {
	// ID of the plan (if any)
	PlanID string
	// Target node of the preemptor (if any)
	TargetNode string
	// Batch size (if any)
	BatchSize int
	// Total pods before plan execution
	TotalPrePlan int
	// Total pods after plan execution
	TotalPostPlan int
	// Chosen solver
	ChosenSolver SolverSummary
	// Total duration of the flow
	TotalDuration time.Duration
}
