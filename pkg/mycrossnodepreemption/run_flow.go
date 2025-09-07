// helpers_runflow.go
package mycrossnodepreemption

import (
	"context"
	"sort"
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
			name:    "bfs",
			enabled: SolverBfsEnabled,
			timeout: SolverBfsTimeout,
			run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverBfs(in), nil
			},
		},
		{
			name:    "swap",
			enabled: SolverSwapEnabled,
			timeout: SolverSwapTimeout,
			run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverSwap(in), nil
			},
		},
		{
			name:    "python",
			enabled: SolverPythonEnabled,
			timeout: SolverPythonTimeout,
			fudgeMs: 200, // let the solver return a feasible result before ctx timeout
			run:     pl.runSolverPython,
		},
	}

	var (
		bestScore    Score
		bestName     string
		bestDuration time.Duration
	)

	// Log enabled solvers and their timeouts once (verbose only).
	enabledNames := []string{}
	timeoutsMs := []int64{}
	for _, a := range attempts {
		if !a.enabled {
			continue
		}
		enabledNames = append(enabledNames, a.name)
		timeoutsMs = append(timeoutsMs, a.timeout.Milliseconds())
	}
	klog.InfoS(string(phase)+": solver attempts planned",
		"enabled", enabledNames, "timeoutsMs", timeoutsMs,
		"baselinePlacedByPri", baseline.PlacedByPriority,
		"baselineEvictions", baseline.Evicted, "baselineMoves", baseline.Moved)

	type attemptResult struct {
		name     string
		duration time.Duration
		score    Score
		cmpBase  int // -1 worse, 0 equal, 1 better
	}
	var results []attemptResult

	for i, att := range attempts {
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

		if err != nil {
			klog.ErrorS(err, string(phase)+": solver failed",
				"attempt", i, "solver", att.name, "duration", attDur, "timeoutMs", att.timeout.Milliseconds(), "timeoutHintMs", toMs)
			continue
		}
		if !IsSolverFeasible(out) {
			klog.InfoS(string(phase)+": solver infeasible",
				"attempt", i, "solver", att.name, "duration", attDur, "timeoutMs", att.timeout.Milliseconds(), "timeoutHintMs", toMs, "status", out.Status)
			continue
		}
		anyFeasible = true
		sc := computeSolverScore(inAttempt, out)
		dEv := sc.Evicted - baseline.Evicted
		dMv := sc.Moved - baseline.Moved

		if bestOut == nil {
			// First feasible candidate: compare vs baseline for logging.
			switch IsImprovement(baseline, sc) {
			case 1:
				klog.InfoS(string(phase)+": solver improved over baseline",
					"attempt", i, "solver", att.name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
			case 0:
				klog.InfoS(string(phase)+": solver equal to baseline",
					"attempt", i, "solver", att.name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			default:
				klog.InfoS(string(phase)+": solver worse than baseline",
					"attempt", i, "solver", att.name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
			}

			bestOut, bestScore, bestName, bestDuration = out, sc, att.name, attDur
			results = append(results, attemptResult{name: att.name, duration: attDur, score: sc, cmpBase: IsImprovement(baseline, sc)})
			continue
		}

		// Compare against the current best.
		switch IsImprovement(bestScore, sc) {
		case 1:
			klog.InfoS(string(phase)+": new leader",
				"attempt", i, "solver", att.name, "prevLeader", bestName, "duration", attDur,
				"leaderPlacedByPri", sc.PlacedByPriority, "prevPlacedByPri", bestScore.PlacedByPriority,
				"leaderEvictions", sc.Evicted, "prevEvictions", bestScore.Evicted,
				"leaderMoves", sc.Moved, "prevMoves", bestScore.Moved)
			bestOut, bestScore, bestName, bestDuration = out, sc, att.name, attDur
		case 0: // if equal; keep the first one
			klog.InfoS(string(phase)+": solver tied with leader",
				"attempt", i, "solver", att.name, "leader", bestName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			bestName = bestName + "=" + att.name + " (" + bestName + " chosen)"
		default:
			klog.InfoS(string(phase)+": solver worse than leader",
				"attempt", i, "solver", att.name, "leader", bestName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "leaderPlacedByPri", bestScore.PlacedByPriority,
				"evictions", sc.Evicted, "leaderEvictions", bestScore.Evicted,
				"moves", sc.Moved, "leaderMoves", bestScore.Moved)
		}
		results = append(results, attemptResult{name: att.name, duration: attDur, score: sc, cmpBase: IsImprovement(baseline, sc)})
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

	// Emit a compact leaderboard (verbose only) across feasible solvers (best → worst vs baseline).
	if len(results) > 0 {
		type row struct {
			Name     string
			Duration time.Duration
			Score    Score
			CmpBase  int
		}
		order := make([]row, 0, len(results))
		for _, r := range results {
			order = append(order, row{r.name, r.duration, r.score, r.cmpBase})
		}
		// Sort: better-than-baseline first (stable among equals by duration asc), then equal, then worse.
		sort.SliceStable(order, func(i, j int) bool {
			if order[i].CmpBase != order[j].CmpBase {
				return order[i].CmpBase > order[j].CmpBase // 1 > 0 > -1
			}
			return order[i].Duration < order[j].Duration
		})

		names := make([]string, len(order))
		evs := make([]int, len(order))
		mvs := make([]int, len(order))
		cmps := make([]int, len(order))
		dursMs := make([]int64, len(order))

		for i := range order {
			names[i] = order[i].Name
			evs[i] = order[i].Score.Evicted
			mvs[i] = order[i].Score.Moved
			cmps[i] = order[i].CmpBase
			dursMs[i] = order[i].Duration.Milliseconds()
		}

		klog.InfoS(string(phase)+": solver leaderboard",
			"order", names,
			"durationsMs", dursMs,
			"evictions", evs,
			"moves", mvs,
			"cmpVsBaseline", cmps,
			"best", bestSolverSummary.Name)
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
	var ap *ActivePlanState
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
