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
	in0, baseline, d0, err := pl.buildInputAndBaseline(solveMode, preemptor, batchedPods, SolverTimeout)
	if err != nil {
		pl.leaveActive()
		return nil, err
	}

	// ---------- Solve with timeout ----------
	solverStart := time.Now()
	ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
	out, err := pl.runSolver(ctxSolve, in0)
	cancel()
	solverDur := time.Since(solverStart)

	// ---------- Feasibility / improvement (+ nomination for single) ----------
	requireNomination := (solveMode == SolveSingle && preemptor != nil)
	if err != nil {
		pl.leaveActive()
		if out != nil {
			// Print baseline and score for debugging
			klog.InfoS("Solver output", "baseline", baseline, "score", out.Score)
			if !IsSolverFeasible(out) {
				klog.ErrorS(ErrNoOptimalOrFeasible, string(phase)+": no optimal or feasible solution")
				return nil, ErrNoOptimalOrFeasible
			} else if !IsImprovement(baseline, out.Score) {
				klog.ErrorS(ErrNoImprovement, string(phase)+": no improvement found")
				return nil, ErrNoImprovement
			} else if requireNomination && out.NominatedNode == "" {
				klog.ErrorS(ErrNoNomination, string(phase)+": no node nominated for preemption")
				return nil, ErrNoNomination
			}
		}
		klog.ErrorS(ErrSolver, "Solver failed")
		return nil, ErrSolver
	}

	// ---------- Digest recheck (cluster drift between build and apply) ----------
	// TODO_HC: digest mismatches occur when running Every mode, possibly due to many changes in short timeframe; or too strict digest checks
	if phase == PhaseContinuous {
		_, _, d1, err := pl.buildInputAndBaseline(solveMode, preemptor, batchedPods, SolverTimeout)
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
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(out)

	// ---------- Register + execute plan ----------
	var plan *Plan
	var ap *ActivePlanState
	if solveMode == SolveSingle {
		plan, ap, err = pl.registerPlan(ctx, out, preemptor) // pass the pod
	} else {
		plan, ap, err = pl.registerPlan(ctx, out, nil)
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
		SolverDuration: solverDur,
	}
	if ap != nil {
		res.PlanID = ap.ID
	}
	if out != nil {
		res.Nominated = out.NominatedNode
		res.SolverStatus = out.Status
	}
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
		"totalDuration", res.TotalDuration,
		"solverDuration", res.SolverDuration,
	)

	return res, nil
}
