// run_solvers.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// helper near the top of run_solvers.go (or anywhere shared)
func cloneScore(s Score) *Score {
	var m map[string]int
	if s.PlacedByPriority != nil {
		m = make(map[string]int, len(s.PlacedByPriority))
		for k, v := range s.PlacedByPriority {
			m[k] = v
		}
	}
	return &Score{
		PlacedByPriority: m,
		Evicted:          s.Evicted,
		Moved:            s.Moved,
	}
}

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	phase Phase,
	in SolverInput,
	baseline Score,
) (chosenOut *SolverOutput, anyFeasible bool, chosenSolverSummary SolverSummary) {
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
			}
		}
		klog.V(V2).InfoS(string(phase)+": direct-fit could not place all pods; run solvers", "duration", time.Since(dfStart))
	}

	// The list is ordered by preference.
	attempts := []SolverAttempt{
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
			Name:    "bfs",
			Enabled: SolverBfsEnabled,
			Timeout: SolverBfsTimeout,
			Trials:  1,
			Run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverCommon(in, bfsPlan, "bfs", base), nil
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
		chosenScore    Score
		chosenName     string
		chosenDuration time.Duration
	)

	currentTarget := baseline

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
		Status   string
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

		// Provide "improve-from-here" thresholds to the solver.
		inAttempt.UseHints = SolverUseHints
		if SolverUseHints {
			inAttempt.Hints = cloneScore(currentTarget)
		} else {
			inAttempt.Hints = nil
		}

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

		if chosenOut == nil {
			// First feasible candidate: compare vs baseline for logging.
			switch IsImprovement(baseline, sc) {
			case 1:
				klog.V(V2).InfoS(string(phase)+": solver improved over baseline",
					"attempt", i, "solver", att.Name, "duration", attDur,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
				currentTarget = sc
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

			chosenOut, chosenScore, chosenName, chosenDuration = out, sc, att.Name, attDur
			results = append(results, AttemptResult{
				Name:     att.Name,
				Duration: attDur,
				Score:    sc,
				CmpBase:  IsImprovement(baseline, sc),
				Status:   out.Status,
			})
			continue
		}

		// Compare against the current best.
		switch IsImprovement(chosenScore, sc) {
		case 1:
			klog.V(V2).InfoS(string(phase)+": new leader",
				"attempt", i, "solver", att.Name, "prevLeader", chosenName, "duration", attDur,
				"leaderPlacedByPri", sc.PlacedByPriority, "prevPlacedByPri", chosenScore.PlacedByPriority,
				"leaderEvictions", sc.Evicted, "prevEvictions", chosenScore.Evicted,
				"leaderMoves", sc.Moved, "prevMoves", chosenScore.Moved)
			chosenOut, chosenScore, chosenName, chosenDuration = out, sc, att.Name, attDur
			if IsImprovement(currentTarget, sc) == 1 {
				currentTarget = sc
			}
		case 0: // tie with current leader; keep the first one as chosen
			klog.V(V2).InfoS(string(phase)+": solver tied with leader",
				"attempt", i, "solver", att.Name, "leader", chosenName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
		default:
			klog.V(V2).InfoS(string(phase)+": solver worse than leader",
				"attempt", i, "solver", att.Name, "leader", chosenName, "duration", attDur,
				"placedByPri", sc.PlacedByPriority, "leaderPlacedByPri", chosenScore.PlacedByPriority,
				"evictions", sc.Evicted, "leaderEvictions", chosenScore.Evicted,
				"moves", sc.Moved, "leaderMoves", chosenScore.Moved)
		}
		results = append(results, AttemptResult{
			Name:     att.Name,
			Duration: attDur,
			Score:    sc,
			CmpBase:  IsImprovement(baseline, sc),
			Status:   out.Status,
		})
	}

	// If python was OPTIMAL and the chosen solver ties python's score,
	// force the chosen solver's status to OPTIMAL in the summary we return/log.
	chosenStatus := chosenOut.Status
	pyIdx := -1
	for i := range results {
		if results[i].Name == "python" {
			pyIdx = i
			break
		}
	}
	if pyIdx >= 0 && results[pyIdx].Status == "OPTIMAL" {
		pyScore := results[pyIdx].Score
		// chosen ties python?
		if IsImprovement(pyScore, chosenScore) == 0 &&
			IsImprovement(chosenScore, pyScore) == 0 {
			chosenStatus = "OPTIMAL"
		}
	}

	// Build best solver summary (if any)
	if chosenOut != nil {
		chosenSolverSummary = SolverSummary{
			Name:     chosenName,
			Status:   chosenStatus,
			Duration: chosenDuration,
			Score:    chosenScore,
		}
	}

	if len(results) > 0 {
		type Row struct {
			Name     string
			Duration time.Duration
			Score    Score
			CmpBase  int
			PrefIdx  int // attempt order index to keep the first one first for true ties
		}

		// Build rows with attempt order as PrefIdx
		ranking := make([]Row, 0, len(results))
		for i, r := range results {
			ranking = append(ranking, Row{
				Name:     r.Name,
				Duration: r.Duration,
				Score:    r.Score,
				CmpBase:  r.CmpBase,
				PrefIdx:  i, // attempt order
			})
		}

		// Partition into groups by CmpBase, keeping insertion (attempt) order.
		better := make([]Row, 0, len(ranking))
		equal := make([]Row, 0, len(ranking))
		worse := make([]Row, 0, len(ranking))
		for _, r := range ranking {
			switch r.CmpBase {
			case 1:
				better = append(better, r)
			case 0:
				equal = append(equal, r)
			default:
				worse = append(worse, r)
			}
		}

		// Concatenate: better → equal → worse, all in original attempt order.
		ranking = append(append(better, equal...), worse...)

		// Build printed arrays + tie markers
		names := make([]string, len(ranking))
		evs := make([]int, len(ranking))
		mvs := make([]int, len(ranking))
		cmps := make([]int, len(ranking))
		dursUs := make([]int64, len(ranking))

		for i := range ranking {
			label := ranking[i].Name
			if i > 0 {
				// Mark as "(tie)" if this score equals the previous score.
				if IsImprovement(ranking[i-1].Score, ranking[i].Score) == 0 &&
					IsImprovement(ranking[i].Score, ranking[i-1].Score) == 0 {
					label += " (tie)"
				}
			}

			names[i] = label
			evs[i] = ranking[i].Score.Evicted
			mvs[i] = ranking[i].Score.Moved
			cmps[i] = ranking[i].CmpBase
			dursUs[i] = ranking[i].Duration.Microseconds()
		}

		klog.InfoS(string(phase)+": solver leaderboard",
			"ranking", names,
			"durationsUs", dursUs,
			"evictions", evs,
			"moves", mvs,
			"placedByPri", chosenSolverSummary.Score.PlacedByPriority)
	}

	// Build attempts for ledger
	evAttempts := make([]SolverAttemptEvent, 0, len(results))
	for _, r := range results {
		evAttempts = append(evAttempts, SolverAttemptEvent{
			Name:       r.Name,
			Status:     r.Status,
			DurationUs: r.Duration.Microseconds(),
			Score:      r.Score,
		})
	}

	var evChosen *SolverAttemptEvent
	if chosenOut != nil {
		evChosen = &SolverAttemptEvent{
			Name:       chosenName,
			Status:     chosenStatus,
			DurationUs: chosenDuration.Microseconds(),
			Score:      chosenScore,
		}
	}

	pl.appendLeaderboardCM(ctx, SolverRunEvent{
		Timestamp_ns: time.Now().UnixNano(),
		Baseline:     baseline,
		Attempts:     evAttempts,
		Chosen:       evChosen,
	})

	return chosenOut, anyFeasible, chosenSolverSummary
}
