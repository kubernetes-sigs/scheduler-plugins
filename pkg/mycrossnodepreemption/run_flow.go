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
			klog.V(V2).InfoS(string(phase) + ": another plan active; skipping")
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
	in0, baseline, d0, err := pl.buildInputAndBaseline(solveMode, nodes, pods, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, string(phase)+": failed to build input/baseline")
		pl.leaveActive()
		return nil, err
	}

	// ---------- Solve ----------
	bestOut, anyFeasible, bestSummary, solverDuration := pl.runSolvers(ctx, phase, in0, baseline)

	// Decide failure reason:
	// - If BOTH solvers are infeasible (or nil) -> ErrNoOptimalOrFeasible
	// - Else if no improvement vs baseline -> ErrNoImprovement
	if !anyFeasible {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, string(phase)+": no optimal/feasible solution from any solver")
		return nil, ErrNoOptimalOrFeasible
	}

	switch IsImprovement(baseline, bestSummary.Score) {
	case 1: // bestScore better than baseline
		// proceed
	case 0: // bestScore equal to baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": equal to baseline (no improvement)",
			"placedByPri", bestSummary.Score.PlacedByPriority,
			"evictions", bestSummary.Score.Evicted,
			"moves", bestSummary.Score.Moved)
		return nil, ErrNoImprovement
	case -1: // bestScore worse than baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": worse than baseline",
			"placedByPri", bestSummary.Score.PlacedByPriority,
			"evictions", bestSummary.Score.Evicted,
			"moves", bestSummary.Score.Moved)
		return nil, ErrNoImprovement
	}

	// Digest recheck when in continuous mode to detect cluster drift between building -> solving -> applying.
	// Applying needs to be at the same state as when we take the digest (cluster state).
	if phase == PhaseContinuous {
		_, _, d1, err := pl.buildInputAndBaseline(solveMode, nodes, pods, preemptor, batchedPods)
		if err != nil {
			pl.leaveActive()
			return nil, err
		}
		if d0 != d1 {
			klog.InfoS(string(phase) + ": digest mismatch pre-apply; skipping")
			pl.leaveActive()
			return nil, ErrDigestMismatch
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
	doc, ap, targetNode, err = pl.registerPlan(ctx, bestOut, bestSummary, preemptor, pods)
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
		PlanID:         ap.ID,
		TargetNode:     targetNode,
		BatchSize:      len(batchedPods),
		Moves:          len(doc.Moves),
		Evicts:         len(doc.Evicts),
		TotalPrePlan:   totalPrePlan,
		TotalPostPlan:  totalPostPlan,
		SolverStatus:   bestSummary.Status,
		TotalDuration:  time.Since(start),
		SolverDuration: solverDuration,
	}
	klog.InfoS(string(phase)+": plan execution finished; waiting for settlement",
		"planID", res.PlanID,
		"nominated", res.TargetNode,
		"batchSize", res.BatchSize,
		"moves", res.Moves,
		"evicts", res.Evicts,
		"totalPrePlan", res.TotalPrePlan,
		"totalPostPlan", res.TotalPostPlan,
		"solverStatus", res.SolverStatus,
		"bestSolver", bestSummary.Name,
		"totalDuration", res.TotalDuration,
		"solverDuration", res.SolverDuration,
	)

	return res, nil
}
