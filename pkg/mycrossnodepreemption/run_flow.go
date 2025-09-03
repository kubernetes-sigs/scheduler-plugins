// helpers_runflow.go
package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// TODO
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, phase Phase, singlePod *v1.Pod) (*FlowResult, error) {
	// Continuous: do NOT take Active yet (we only take it if there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if phase != PhaseContinuous {
		if !pl.tryEnterActive() {
			klog.V(V2).InfoS(string(phase) + ": another plan active; skipping")
			return nil, ErrActiveInProgress
		}
	}

	// Make sure we have the latest node and pod information
	// TODO: wait for pod and node cache has synced instead of sleeping
	time.Sleep(300 * time.Millisecond)

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
		_ = pl.pruneStaleSetEntries(pl.Batched)
		batchedPods = pl.snapshotBatch()
		if len(batchedPods) == 0 {
			klog.InfoS(string(phase) + ": no batched pod(s) to schedule")
			pl.leaveActive()
			return nil, ErrNoop
		}
	default: // PreEnqueue / PostFilter single-preemptor flow
		solveMode = SolveSingle
		preemptor = singlePod
	}

	// ---------- Build input + baseline + digest ----------
	in0, baseline, d0, err := pl.buildInputAndBaseline(solveMode, preemptor, batchedPods)
	if err != nil {
		klog.ErrorS(err, string(phase)+": failed to build input/baseline")
		pl.leaveActive()
		return nil, err
	}

	// ---------- Solve with heuristic first, then (optionally) Python ----------
	solverStart := time.Now()

	// Candidate best solution (from heuristic and/or python)
	var bestOut *SolverOutput
	fastFeasible := false
	pyFeasible := false
	bestSolver := "none" // "heuristic" or "python"

	// 1) Optional fast (greedy) solver
	if SolverFastEnabled {
		in0.TimeoutMs = SolverFastTimeout.Milliseconds() // TODO: Make use of in.TimeoutMs
		fastOut := runFastSolver(in0)
		fastFeasible = fastOut != nil && IsSolverFeasible(fastOut)
		bestOut = fastOut
		if fastFeasible {
			switch IsImprovement(baseline, fastOut.Score) {
			case 1:
				klog.V(V2).InfoS(string(phase)+": fast solver improved over baseline",
					"placedByPri", fastOut.Score.PlacedByPriority,
					"evictions", fastOut.Score.Evicted,
					"moves", fastOut.Score.Moved)
				bestSolver = "fast"
			case 0:
				klog.V(V2).InfoS(string(phase)+": fast solver equal to baseline",
					"placedByPri", fastOut.Score.PlacedByPriority,
					"evictions", fastOut.Score.Evicted,
					"moves", fastOut.Score.Moved)
			case -1:
				klog.V(V2).InfoS(string(phase)+": fast solver worse than baseline",
					"placedByPri", fastOut.Score.PlacedByPriority,
					"evictions", fastOut.Score.Evicted,
					"moves", fastOut.Score.Moved)
			}
		}
	}

	// 2) Python solver (only keep if it improves over heuristic)
	if SolverPythonEnabled {
		ctxSolve, cancel := context.WithTimeout(ctx, SolverPythonTimeout)
		in0.TimeoutMs = SolverPythonTimeout.Milliseconds() - 200 // substract 200 ms to let the solver be able to return a feasible result.
		pyOut, pyErr := pl.runSolver(ctxSolve, in0)
		cancel()
		pyFeasible = pyErr == nil && IsSolverFeasible(pyOut)
		if bestOut == nil {
			if pyFeasible {
				bestOut = pyOut
				bestSolver = "python"
				switch IsImprovement(baseline, pyOut.Score) { // baseline vs. python
				case 1:
					klog.V(V2).InfoS(string(phase)+": python improved over baseline",
						"placedByPri", pyOut.Score.PlacedByPriority,
						"evictions", pyOut.Score.Evicted,
						"moves", pyOut.Score.Moved)
				case 0:
					klog.V(V2).InfoS(string(phase)+": python equal to baseline",
						"placedByPri", pyOut.Score.PlacedByPriority,
						"evictions", pyOut.Score.Evicted,
						"moves", pyOut.Score.Moved)
				default:
					klog.V(V2).InfoS(string(phase)+": python worse than baseline",
						"placedByPri", pyOut.Score.PlacedByPriority,
						"evictions", pyOut.Score.Evicted,
						"moves", pyOut.Score.Moved)
				}
			}
		} else if pyFeasible {
			switch IsImprovement(bestOut.Score, pyOut.Score) { // best (i.e. fast solver) vs. python
			case 1:
				klog.InfoS(string(phase)+": python improved over fast solver",
					"placedByPri", pyOut.Score.PlacedByPriority,
					"evictions", pyOut.Score.Evicted,
					"moves", pyOut.Score.Moved)
				bestOut = pyOut
				bestSolver = "python"
			case 0:
				klog.InfoS(string(phase)+": python equal to fast solver",
					"placedByPri", pyOut.Score.PlacedByPriority,
					"evictions", pyOut.Score.Evicted,
					"moves", pyOut.Score.Moved)
				bestSolver = "python=fast"
			case -1:
				klog.InfoS(string(phase)+": python worse than fast solver",
					"placedByPri", pyOut.Score.PlacedByPriority,
					"evictions", pyOut.Score.Evicted,
					"moves", pyOut.Score.Moved)
			}
		}
	}

	solverDuration := time.Since(solverStart)

	// Decide failure reason:
	// - If BOTH solvers are infeasible (or nil) -> ErrNoOptimalOrFeasible
	// - Else if no improvement vs baseline -> ErrNoImprovement
	if !(fastFeasible || pyFeasible) {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, string(phase)+": no optimal/feasible solution from any solver")
		return nil, ErrNoOptimalOrFeasible
	}

	switch IsImprovement(baseline, bestOut.Score) {
	case 1: // bestOut better than baseline
		// proceed
	case 0: // bestOut equal to baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": equal to baseline (no improvement)",
			"placedByPri", bestOut.Score.PlacedByPriority,
			"evictions", bestOut.Score.Evicted,
			"moves", bestOut.Score.Moved)
		return nil, ErrNoImprovement
	case -1: // bestOut worse than baseline
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": worse than baseline",
			"placedByPri", bestOut.Score.PlacedByPriority,
			"evictions", bestOut.Score.Evicted,
			"moves", bestOut.Score.Moved)
		return nil, ErrNoImprovement
	}

	// Digest recheck when in continuous mode to detect cluster drift between building -> solving -> applying.
	// Applying needs to be at the same state as when we take the digest (cluster state).
	if phase == PhaseContinuous {
		_, _, d1, err := pl.buildInputAndBaseline(solveMode, preemptor, batchedPods)
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
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestOut)

	// ---------- Register + execute plan ----------
	var plan *Plan
	var ap *ActivePlanState
	if solveMode == SolveSingle {
		plan, ap, err = pl.registerPlan(ctx, bestOut, preemptor) // pass the pod
	} else {
		plan, ap, err = pl.registerPlan(ctx, bestOut, nil)
	}
	if err != nil {
		// For single-preemptor, keep it blocked if we failed to register/execute.
		if solveMode == SolveSingle && preemptor != nil {
			pl.Blocked.AddPod(preemptor)
		}
		klog.ErrorS(err, string(phase)+": register plan failed")
		pl.leaveActive()
		return nil, ErrRegisterPlan
	}

	if pendingScheduled == 0 {
		klog.InfoS(string(phase) + ": no pending pod(s) to be scheduled; skipping")
		pl.onPlanSettled()
		return nil, ErrNoop
	}

	// Only execute if there are moves or evicts
	if plan != nil && (len(plan.Moves) > 0 || len(plan.Evicts) > 0) {
		if err := pl.executePlan(ctx, plan); err != nil {
			klog.ErrorS(err, "Plan execution failed")
			pl.onPlanSettled()
		}
	}

	if phase == PhaseBatch {
		pl.activateBatchedPods(batchedPods, 0)
	}

	res := &FlowResult{
		PlanID:         "",
		Nominated:      "",
		BatchSize:      len(batchedPods),
		Moves:          0,
		Evicts:         0,
		TotalPrePlan:   totalPrePlan,
		TotalPostPlan:  totalPostPlan,
		SolverStatus:   "",
		TotalDuration:  time.Since(start),
		SolverDuration: solverDuration,
	}
	if ap != nil {
		res.PlanID = ap.ID
	}
	res.Nominated = bestOut.NominatedNode
	res.SolverStatus = bestOut.Status
	if plan != nil {
		res.Moves = len(plan.Moves)
		res.Evicts = len(plan.Evicts)
	}

	klog.InfoS(string(phase)+": plan execution finished; waiting for settlement",
		"planID", res.PlanID,
		"nominated", res.Nominated,
		"batchSize", res.BatchSize,
		"moves", res.Moves,
		"evicts", res.Evicts,
		"totalPrePlan", res.TotalPrePlan,
		"totalPostPlan", res.TotalPostPlan,
		"solverStatus", res.SolverStatus,
		"bestSolver", bestSolver,
		"totalDuration", res.TotalDuration,
		"solverDuration", res.SolverDuration,
	)

	return res, nil
}
