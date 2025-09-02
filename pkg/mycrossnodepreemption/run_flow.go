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
		bestSolver = "heuristic"
	} else {
		klog.V(V2).InfoS(string(phase) + ": fast solver disabled")
	}

	// 2) Python solver (only keep if it improves over heuristic)
	if SolverPythonEnabled {
		ctxSolve, cancel := context.WithTimeout(ctx, SolverPythonTimeout)
		in0.TimeoutMs = SolverPythonTimeout.Milliseconds()
		pyOut, pyErr := pl.runSolver(ctxSolve, in0)
		cancel()
		pyFeasible = pyErr == nil && IsSolverFeasible(pyOut)
		// If we didn't run the fast solver (or it was disabled), accept python if feasible.
		if bestOut == nil {
			if pyFeasible {
				bestOut = pyOut
				bestSolver = "python"
			}
		} else if pyFeasible && IsImprovement(bestOut.Score, pyOut.Score) {
			// Otherwise, keep python only if it improves over the current best.
			bestOut = pyOut
			bestSolver = "python"
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
	if !IsImprovement(baseline, bestOut.Score) {
		pl.leaveActive()
		klog.ErrorS(ErrNoImprovement, string(phase)+": no improvement over baseline")
		return nil, ErrNoImprovement
	}

	// From here on, use bestOut
	klog.InfoS(string(phase)+": best solver", "name", bestSolver)

	// ---------- Feasibility / improvement (+ nomination for single) ----------
	if bestOut == nil || !IsSolverFeasible(bestOut) {
		pl.leaveActive()
		klog.ErrorS(ErrNoOptimalOrFeasible, string(phase)+": no optimal or feasible solution")
		return nil, ErrNoOptimalOrFeasible
	}

	// ---------- Digest recheck (cluster drift between build and apply) ----------
	// TODO_HC: digest mismatches occur when running Every mode, possibly due to many changes in short timeframe; or too strict digest checks
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
