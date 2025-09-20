// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the full flow for the given phase (Continuous, Batch, Single).
// For Single phase, the singlePod must be provided (the preemptor).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, phase Phase, singlePod *v1.Pod) (*FlowResult, error) {
	// Continuous: do NOT take Active yet (we only take it if there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if phase != PhaseContinuous {
		if !pl.tryEnterActive() {
			klog.V(MyVerbosity).InfoS(string(phase) + ": another plan active; skipping")
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

	switch phase {
	case PhaseContinuous:
		solveMode = SolveContinuously
	case PhaseBatch:
		solveMode = SolveBatch
		_ = pl.pruneSetEntries(pl.Batched)
		batchedPods = pl.snapshotBatch()
		if len(batchedPods) == 0 {
			klog.InfoS(string(phase) + ": no batched pod(s) to schedule")
			pl.leaveActive()
			return nil, ErrNoop
		}
	default: // Every: PreEnqueue / PostFilter
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// -------- Fetch nodes and pods ONCE for this flow --------
	nodes, err := pl.getNodes()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, string(phase)+": failed to list nodes")
		return nil, err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.ErrorS(err, string(phase)+": failed to list pods")
		return nil, err
	}
	// ----------------------------------------------------

	// ---------- Build input + baseline + digest ----------
	in0, baseline, err := pl.buildInputAndBaseline(solveMode, nodes, pods, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, string(phase)+": failed to build input/baseline")
		pl.leaveActive()
		return nil, err
	}

	// ---------- Solve ----------
	bestOut, anyFeasible, chosenSolver := pl.runSolvers(ctx, phase, in0, baseline)

	// Decide failure reason:
	// - If BOTH solvers are infeasible (or nil) -> ErrNoOptimalOrFeasible
	// - Else if no improvement vs baseline -> ErrNoImprovement
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, string(phase)+": no optimal/feasible solution from any solver")
		return nil, ErrNoOptimalOrFeasible
	}

	switch IsImprovement(baseline, chosenSolver.Score) {
	case 1: // bestScore better than baseline
		// proceed
	case 0: // bestScore equal to baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": equal to baseline (no improvement)",
			"placedByPri", chosenSolver.Score.PlacedByPriority,
			"evictions", chosenSolver.Score.Evicted,
			"moves", chosenSolver.Score.Moved)
		return nil, ErrNoImprovement
	case -1: // bestScore worse than baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": worse than baseline",
			"placedByPri", chosenSolver.Score.PlacedByPriority,
			"evictions", chosenSolver.Score.Evicted,
			"moves", chosenSolver.Score.Moved)
		return nil, ErrNoImprovement
	}

	// In continuous mode, allow benign drift; only skip if the plan is no longer applicable.
	if phase == PhaseContinuous {
		ok, why := pl.planStillApplicable(bestOut, nodes, pods)
		if !ok {
			klog.InfoS("Continuous: plan no longer applicable; skipping", "reason", why)
			pl.leaveActive()
			return nil, ErrDigestMismatch // reuse error; message logs the reason
		}
	}

	// ---------- Take Active late for Continuous (only now that we know it's worth applying) ----------
	if phase == PhaseContinuous {
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
		klog.ErrorS(err, string(phase)+": register plan failed")
		pl.leaveActive()
		return nil, ErrRegisterPlan
	}

	if pendingScheduled == 0 {
		klog.InfoS(string(phase) + ": no pending pod(s) to be scheduled; skipping")
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

	if phase == PhaseBatch {
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
	klog.InfoS(string(phase)+": plan execution finished; waiting for settlement",
		"planID", res.PlanID,
		"chosenSolver", res.ChosenSolver,
		"nominated", res.TargetNode,
		"batchSize", res.BatchSize,
		"totalPrePlan", res.TotalPrePlan,
		"totalDuration", res.TotalDuration,
	)

	return res, nil
}
