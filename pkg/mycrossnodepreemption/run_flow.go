// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// TODO: Reach to here in this file...

// runFlow runs the full flow for the given phase (Continuous, Batch, Single).
// For Single phase, the singlePod must be provided (the preemptor).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, singlePod *v1.Pod) (*FlowResult, error) {
	label := strategyToString()

	// Continuous: do NOT take Active yet (we only take it if there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(label + ": another plan active; skipping")
			return nil, ErrActiveInProgress
		}
	}

	start := time.Now()

	// ---------- Phase-specific setup ----------
	var (
		solveMode   SolveMode
		preemptor   *v1.Pod
		batchedPods []*v1.Pod
	)
	if optimizeContinuous() {
		solveMode = SolveContinuous
	} else if optimizeBatch() {
		solveMode = SolveBatch
		_ = pl.pruneSetEntries(pl.Batched)
		batchedPods = pl.snapshotBatch()
		if len(batchedPods) == 0 {
			klog.InfoS(label + ": no batched pod(s) to schedule")
			pl.leaveActive()
			return nil, ErrNoop
		}
	} else { // Every: PreEnqueue / PostFilter
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// -------- Fetch nodes and pods ONCE for this flow --------
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

	// ---------- Build input ----------
	solverInput, err := pl.buildSolverInput(solveMode, nodes, pods, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, label+": failed to build solver input")
		pl.leaveActive()
		return nil, err
	}

	// ---------- Solve ----------
	bestOut, anyFeasible, chosenSolver := pl.runSolvers(ctx, solverInput, nodes, pods)

	// Decide failure reason:
	// - If BOTH solvers are infeasible (or nil) -> ErrNoOptimalOrFeasible
	// - Else if no improvement vs baseline -> ErrNoImprovement
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, label+": no optimal/feasible solution from any solver")
		return nil, ErrNoOptimalOrFeasible
	}

	// In continuous mode, allow benign drift; only skip if the plan is no longer applicable.
	ok, why := pl.planApplicable(bestOut, nodes, pods)
	if !ok {
		klog.InfoS("Plan is not applicable; skipping", "reason", why)
		pl.leaveActive()
		return nil, ErrDigestMismatch // reuse error; message logs the reason
	}

	// ---------- Take Active late for Continuous (only now that we know it's worth applying) ----------
	if optimizeContinuous() {
		if !pl.tryEnterActive() {
			klog.InfoS("Continuous: another plan active; skipping")
			return nil, ErrActiveInProgress
		}
	}

	// ---------- Count new and total pods ----------
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestOut, pods)

	// ---------- Register + execute plan ----------
	var doc *StoredPlan
	var ap *ActivePlan
	var targetNode string
	doc, ap, targetNode, err = pl.registerPlan(ctx, bestOut, chosenSolver, preemptor, pods)
	if err != nil {
		// keep single-preemptor blocked on error
		if solveMode == SolveSingle && preemptor != nil {
			pl.Blocked.AddPod(preemptor)
		}
		klog.ErrorS(err, label+": register plan failed")
		pl.leaveActive()
		return nil, ErrRegisterPlan
	}

	if pendingScheduled == 0 {
		klog.InfoS(label + ": no pending pod(s) to be scheduled; skipping")
		pl.onPlanSettled(PlanStatusFailed)
		return nil, ErrNoop
	}

	// Execute if there are moves/evictions
	if len(doc.Moves) > 0 || len(doc.Evicts) > 0 {
		if err := pl.executePlan(doc); err != nil {
			klog.ErrorS(err, "Plan execution failed")
			pl.onPlanSettled(PlanStatusFailed)
		}
	}

	if optimizeBatch() {
		pl.activateBatchedPods(batchedPods, 0)
	}

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
