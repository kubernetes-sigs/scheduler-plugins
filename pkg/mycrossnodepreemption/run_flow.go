// helpers_runflow.go
package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type solverAttempt struct {
	name    string
	enabled bool
	timeout time.Duration
	fudgeMs int64
	run     func(ctx context.Context, in SolverInput) (*SolverOutput, error)
}

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	phase Phase,
	in SolverInput,
	baseline Score,
) (bestOut *SolverOutput, anyFeasible bool, bestSolverSummary SolverSummary, totalDuration time.Duration) {
	start := time.Now()

	attempts := []solverAttempt{
		{
			name:    "fast",
			enabled: SolverFastEnabled,
			timeout: SolverFastTimeout,
			run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runFastSolver(in), nil
			},
		},
		{
			name:    "python",
			enabled: SolverPythonEnabled,
			timeout: SolverPythonTimeout,
			fudgeMs: 200, // let the solver return a feasible result before ctx timeout
			run:     pl.runPythonSolver,
		},
	}

	var (
		bestScore    Score
		bestName     string
		bestDuration time.Duration
	)

	for _, att := range attempts {
		if !att.enabled {
			continue
		}
		// copy input and set per-attempt timeout hint
		inAttempt := in
		toMs := att.timeout.Milliseconds()
		if att.fudgeMs > 0 && toMs > att.fudgeMs {
			toMs -= att.fudgeMs
		}
		inAttempt.TimeoutMs = toMs

		ctxAtt, cancel := context.WithTimeout(ctx, att.timeout)
		attStart := time.Now()
		out, err := att.run(ctxAtt, inAttempt)
		cancel()
		attDur := time.Since(attStart)

		feasible := (err == nil) && IsSolverFeasible(out)
		if !feasible {
			continue
		}
		anyFeasible = true
		sc := computeSolverScore(inAttempt, out)

		if bestOut == nil {
			// First feasible candidate: compare vs baseline for logging.
			switch IsImprovement(baseline, sc) {
			case 1:
				klog.V(V2).InfoS(string(phase)+": "+att.name+" improved over baseline",
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			case 0:
				klog.V(V2).InfoS(string(phase)+": "+att.name+" equal to baseline",
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			default:
				klog.V(V2).InfoS(string(phase)+": "+att.name+" worse than baseline",
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			}
			bestOut, bestScore, bestName, bestDuration = out, sc, att.name, attDur
			continue
		}

		// Compare against the current best.
		switch IsImprovement(bestScore, sc) {
		case 1:
			klog.InfoS(string(phase)+": "+att.name+" improved over "+bestName,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			bestOut, bestScore, bestName, bestDuration = out, sc, att.name, attDur
		case 0:
			klog.InfoS(string(phase)+": "+att.name+" equal to "+bestName,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			bestName = bestName + "=" + att.name
		default:
			klog.InfoS(string(phase)+": "+att.name+" worse than "+bestName,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
		}
	}

	// Build best solver summary (if any)
	if bestOut != nil {
		bestSolverSummary = SolverSummary{
			Name:     bestName,
			Status:   bestOut.Status,
			Duration: bestDuration,
			Score:    bestScore,
		}
	}

	return bestOut, anyFeasible, bestSolverSummary, time.Since(start)
}

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

	// ---------- Solve with fast then python (deduped) ----------
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
	var doc *StoredPlan
	var ap *ActivePlanState
	var targetNode string
	// preemptor may be nil. registerPlan should store bestSummary into StoredPlan.Solver.
	doc, ap, targetNode, err = pl.registerPlan(ctx, bestOut, bestSummary, preemptor)
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
		if err := pl.executePlan(ctx, doc); err != nil {
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
