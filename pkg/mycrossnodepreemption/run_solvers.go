package mycrossnodepreemption

import (
	"context"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	phase Phase,
	in SolverInput,
	baseline Score,
) (bestOut *SolverOutput, anyFeasible bool, bestSolverSummary SolverSummary, totalDuration time.Duration) {
	start := time.Now()

	// Build cluster state
	base := prepareState(in)

	if optimizeAtPreEnqueue() && phase == PhasePreEnqueue {
		// Direct-fit pre-pass
		dfStart := time.Now()
		if dfOut := runSolverDirectFit(in, base); IsSolverFeasible(dfOut) {
			dfScore := computeSolverScore(in, dfOut)
			dfDur := time.Since(dfStart)
			klog.InfoS(string(phase)+": direct-fit; skipping other solvers",
				"placedByPri", dfScore.PlacedByPriority, "evictions", dfScore.Evicted, "moves", dfScore.Moved,
				"duration", dfDur)
			return dfOut, true, SolverSummary{
				Name:     "direct-fit",
				Status:   dfOut.Status,
				Duration: dfDur,
				Score:    dfScore,
			}, time.Since(start)
		}
		klog.V(V2).InfoS(string(phase)+": direct-fit could not place all pods; run solvers", "duration", time.Since(dfStart))
	}

	attempts := []SolverAttempt{
		{
			Name:    "bfs",
			Enabled: SolverBfsEnabled,
			Timeout: SolverBfsTimeout,
			Trials:  1,
			Run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverCommon(in, bfsPlan, "bfs", base), nil
			},
		},
		{
			Name:    "local-search",
			Enabled: SolverLocalSearchEnabled,
			Timeout: SolverLocalSearchTimeout,
			Trials:  SolverLocalSearchMaxRestartsPerTarget,
			Run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverCommon(in, localSearchPlan, "local-search", base), nil
			},
		},
		{
			Name:    "python",
			Enabled: SolverPythonEnabled,
			Timeout: SolverPythonTimeout,
			Trials:  1,
			FudgeMs: 200,
			Run:     pl.runSolverPython, // python uses raw JSON input
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
		if !a.Enabled {
			continue
		}
		enabledNames = append(enabledNames, a.Name)
		timeoutsMs = append(timeoutsMs, a.Timeout.Milliseconds())
	}
	klog.V(V2).InfoS(string(phase)+": solver attempts planned",
		"enabled", enabledNames, "timeoutsMs", timeoutsMs,
		"baselinePlacedByPri", baseline.PlacedByPriority,
		"baselineEvictions", baseline.Evicted, "baselineMoves", baseline.Moved)

	type AttemptResult struct {
		Name     string
		Duration time.Duration
		Score    Score
		CmpBase  int // -1 worse, 0 equal, 1 better
	}
	var results []AttemptResult

	for i, att := range attempts {
		if !att.Enabled {
			continue
		}
		// copy input and set per-attempt timeout hint
		inAttempt := in
		toMs := att.Timeout.Milliseconds()
		if att.FudgeMs > 0 && toMs > att.FudgeMs {
			toMs -= att.FudgeMs
		}
		inAttempt.TimeoutMs = toMs
		inAttempt.MaxTrials = att.Trials

		ctxAtt, cancel := context.WithTimeout(ctx, att.Timeout)
		attStart := time.Now()
		out, err := att.Run(ctxAtt, inAttempt)
		cancel()
		attDur := time.Since(attStart)

		if err != nil {
			klog.ErrorS(err, string(phase)+": solver failed",
				"attempt", i, "solver", att.Name, "duration", attDur, "timeoutMs", att.Timeout.Milliseconds(), "timeoutHintMs", toMs)
			continue
		}
		if !IsSolverFeasible(out) {
			klog.InfoS(string(phase)+": solver infeasible",
				"attempt", i, "solver", att.Name, "duration", attDur, "timeoutMs", att.Timeout.Milliseconds(), "timeoutHintMs", toMs, "status", out.Status)
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
				klog.V(V2).InfoS(string(phase)+": solver improved over baseline",
					"attempt", i, "solver", att.Name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
			case 0:
				klog.V(V2).InfoS(string(phase)+": solver equal to baseline",
					"attempt", i, "solver", att.Name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			default:
				klog.V(V2).InfoS(string(phase)+": solver worse than baseline",
					"attempt", i, "solver", att.Name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
			}

			bestOut, bestScore, bestName, bestDuration = out, sc, att.Name, attDur
			results = append(results, AttemptResult{Name: att.Name, Duration: attDur, Score: sc, CmpBase: IsImprovement(baseline, sc)})
			continue
		}

		// Compare against the current best.
		switch IsImprovement(bestScore, sc) {
		case 1:
			klog.V(V2).InfoS(string(phase)+": new leader",
				"attempt", i, "solver", att.Name, "prevLeader", bestName, "duration", attDur,
				"leaderPlacedByPri", sc.PlacedByPriority, "prevPlacedByPri", bestScore.PlacedByPriority,
				"leaderEvictions", sc.Evicted, "prevEvictions", bestScore.Evicted,
				"leaderMoves", sc.Moved, "prevMoves", bestScore.Moved)
			bestOut, bestScore, bestName, bestDuration = out, sc, att.Name, attDur
		case 0: // if equal; keep the first one
			klog.V(V2).InfoS(string(phase)+": solver tied with leader",
				"attempt", i, "solver", att.Name, "leader", bestName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			bestName = bestName + "=" + att.Name + " (" + bestName + " chosen)"
		default:
			klog.V(V2).InfoS(string(phase)+": solver worse than leader",
				"attempt", i, "solver", att.Name, "leader", bestName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "leaderPlacedByPri", bestScore.PlacedByPriority,
				"evictions", sc.Evicted, "leaderEvictions", bestScore.Evicted,
				"moves", sc.Moved, "leaderMoves", bestScore.Moved)
		}
		results = append(results, AttemptResult{Name: att.Name, Duration: attDur, Score: sc, CmpBase: IsImprovement(baseline, sc)})
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
		type Row struct {
			Name     string
			Duration time.Duration
			Score    Score
			CmpBase  int
		}
		order := make([]Row, 0, len(results))
		for _, r := range results {
			order = append(order, Row{r.Name, r.Duration, r.Score, r.CmpBase})
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
			"best", bestSolverSummary.Name,
			"evictions", evs,
			"moves", mvs,
			"placedByPri", bestSolverSummary.Score.PlacedByPriority)
	}

	return bestOut, anyFeasible, bestSolverSummary, time.Since(start)
}
