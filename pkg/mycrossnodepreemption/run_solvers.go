// run_solvers.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// TODO: Reach to here in this file...

// helper near the top of run_solvers.go (or anywhere shared)
func cloneScore(s SolverScore) *SolverScore {
	var m map[string]int
	if s.PlacedByPriority != nil {
		m = make(map[string]int, len(s.PlacedByPriority))
		for k, v := range s.PlacedByPriority {
			m[k] = v
		}
	}
	return &SolverScore{
		PlacedByPriority: m,
		Evicted:          s.Evicted,
		Moved:            s.Moved,
	}
}

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	in SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
) (chosenOut *SolverOutput, anyFeasible bool, chosenSolverSummary SolverSummary) {
	label := strategyToString()

	// Build cluster state
	baselineScore := buildBaselineScore(in)
	baseState := buildState(in)

	if optimizeAtPreEnqueue() {
		// Direct-fit pre-pass
		dfStart := time.Now()
		if dfOut := runSolverDirectFit(in, baseState); IsSolverFeasible(dfOut) {
			dfScore := computeSolverScore(in, dfOut)
			dfDurUs := time.Since(dfStart).Microseconds()
			klog.InfoS(label+": direct-fit; skipping other solvers",
				"placedByPri", dfScore.PlacedByPriority, "evictions", dfScore.Evicted, "moves", dfScore.Moved,
				"durationUs", dfDurUs)
			return dfOut, true, SolverSummary{
				Name:       "direct-fit",
				Status:     dfOut.Status,
				DurationUs: dfDurUs,
				Score:      dfScore,
			}
		}
		klog.V(MyVerbosity).InfoS(label+": direct-fit could not place all pods; run solvers", "durationUs", time.Since(dfStart).Microseconds())
	}

	// The list is ordered by preference.
	attempts := []SolverAttempt{
		{
			Name:    "local-search",
			Enabled: SolverLocalSearchEnabled,
			Timeout: SolverLocalSearchTimeout,
			Trials:  SolverLocalSearchMaxRestartsPerTarget,
			Run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverCommon(in, localSearchPlan, "local-search", baseState), nil
			},
		},
		{
			Name:    "bfs",
			Enabled: SolverBfsEnabled,
			Timeout: SolverBfsTimeout,
			Trials:  1,
			Run: func(_ context.Context, in SolverInput) (*SolverOutput, error) {
				return runSolverCommon(in, bfsPlan, "bfs", baseState), nil
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
		chosenScore      SolverScore
		chosenName       string
		chosenDurationUs int64
	)

	currentTarget := baselineScore

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
	klog.V(MyVerbosity).InfoS(label+": solver attempts planned",
		"enabled", enabledNames, "timeoutsMs", timeoutsMs,
		"baselinePlacedByPri", baselineScore.PlacedByPriority,
		"baselineEvictions", baselineScore.Evicted, "baselineMoves", baselineScore.Moved)

	type AttemptResult struct {
		Name       string
		DurationUs int64 // microseconds
		Score      SolverScore
		CmpBase    int // -1 worse, 0 equal, 1 better
		Status     string
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
		attDurUs := time.Since(attStart).Microseconds()

		if err != nil {
			klog.ErrorS(err, label+": solver failed",
				"attempt", i, "solver", att.Name, "durationUs", attDurUs, "timeoutMs", att.Timeout.Milliseconds(), "timeoutHintMs", toMs)
			continue
		}
		if !IsSolverFeasible(out) {
			klog.InfoS(label+": solver infeasible",
				"attempt", i, "solver", att.Name, "durationUs", attDurUs, "timeoutMs", att.Timeout.Milliseconds(), "timeoutHintMs", toMs, "status", out.Status)
			continue
		}
		ok, why := pl.planApplicable(out, nodes, pods)
		if !ok {
			klog.InfoS("Plan from solver is not applicable; skipping", "reason", why,
				"attempt", i, "solver", att.Name, "durationUs", attDurUs, "timeoutMs", att.Timeout.Milliseconds(), "timeoutHintMs", toMs)
			continue
		}

		anyFeasible = true
		sc := computeSolverScore(inAttempt, out)
		dEv := sc.Evicted - baselineScore.Evicted
		dMv := sc.Moved - baselineScore.Moved

		if chosenOut == nil {
			// First feasible candidate: compare vs baseline for logging.
			switch IsImprovement(baselineScore, sc) {
			case 1:
				klog.V(MyVerbosity).InfoS(label+": solver improved over baseline",
					"attempt", i, "solver", att.Name, "durationUs", attDurUs,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
				currentTarget = sc
			case 0:
				klog.V(MyVerbosity).InfoS(label+": solver equal to baseline",
					"attempt", i, "solver", att.Name, "durationUs", attDurUs,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
			default:
				klog.V(MyVerbosity).InfoS(label+": solver worse than baseline",
					"attempt", i, "solver", att.Name, "durationUs", attDurUs,
					"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved,
					"deltaEvictions", dEv, "deltaMoves", dMv)
			}

			chosenOut, chosenScore, chosenName, chosenDurationUs = out, sc, att.Name, attDurUs
			results = append(results, AttemptResult{
				Name:       att.Name,
				DurationUs: attDurUs,
				Score:      sc,
				CmpBase:    IsImprovement(baselineScore, sc),
				Status:     out.Status,
			})
			continue
		}

		// Compare against the current best.
		switch IsImprovement(chosenScore, sc) {
		case 1:
			klog.V(MyVerbosity).InfoS(label+": new leader",
				"attempt", i, "solver", att.Name, "prevLeader", chosenName, "durationUs", attDurUs,
				"leaderPlacedByPri", sc.PlacedByPriority, "prevPlacedByPri", chosenScore.PlacedByPriority,
				"leaderEvictions", sc.Evicted, "prevEvictions", chosenScore.Evicted,
				"leaderMoves", sc.Moved, "prevMoves", chosenScore.Moved)
			chosenOut, chosenScore, chosenName, chosenDurationUs = out, sc, att.Name, attDurUs
			if IsImprovement(currentTarget, sc) == 1 {
				currentTarget = sc
			}
		case 0: // tie with current leader; keep the first one as chosen
			klog.V(MyVerbosity).InfoS(label+": solver tied with leader",
				"attempt", i, "solver", att.Name, "leader", chosenName, "durationUs", attDurUs,
				"placedByPri", sc.PlacedByPriority, "evictions", sc.Evicted, "moves", sc.Moved)
		default:
			klog.V(MyVerbosity).InfoS(label+": solver worse than leader",
				"attempt", i, "solver", att.Name, "leader", chosenName, "durationUs", attDurUs,
				"placedByPri", sc.PlacedByPriority, "leaderPlacedByPri", chosenScore.PlacedByPriority,
				"evictions", sc.Evicted, "leaderEvictions", chosenScore.Evicted,
				"moves", sc.Moved, "leaderMoves", chosenScore.Moved)
		}
		results = append(results, AttemptResult{
			Name:       att.Name,
			DurationUs: attDurUs,
			Score:      sc,
			CmpBase:    IsImprovement(baselineScore, sc),
			Status:     out.Status,
		})
	}

	// If python was OPTIMAL and any of {local-search,bfs} tie python's score,
	// upgrade their status to OPTIMAL. If the chosen solver is one of them,
	// also upgrade chosenOut.Status so callers see OPTIMAL.
	chosenStatus := ""
	if chosenOut != nil {
		chosenStatus = chosenOut.Status
	}
	pyIdx := -1
	for i := range results {
		if results[i].Name == "python" {
			pyIdx = i
			break
		}
	}
	if pyIdx >= 0 && results[pyIdx].Status == "OPTIMAL" {
		pyScore := results[pyIdx].Score
		tiesPython := func(s SolverScore) bool {
			return IsImprovement(pyScore, s) == 0 && IsImprovement(s, pyScore) == 0
		}
		for i := range results {
			if results[i].Name != "python" && tiesPython(results[i].Score) {
				results[i].Status = "OPTIMAL"
			}
		}
		// If our chosen solver is local-search/bfs and ties python, mark it OPTIMAL.
		if chosenOut != nil && (chosenName != "python") && tiesPython(chosenScore) {
			chosenStatus = "OPTIMAL"
			chosenOut.Status = "OPTIMAL"
		}
	}

	// Build best solver summary (if any)
	if chosenOut != nil {
		chosenSolverSummary = SolverSummary{
			Name:       chosenName,
			Status:     chosenStatus,
			DurationUs: chosenDurationUs,
			Score:      chosenScore,
		}
	}

	if len(results) > 0 {
		type Row struct {
			Name       string
			DurationUs int64
			Score      SolverScore
			CmpBase    int
			PrefIdx    int // attempt order index to keep the first one first for true ties
		}

		// Build rows with attempt order as PrefIdx
		ranking := make([]Row, 0, len(results))
		for i, r := range results {
			ranking = append(ranking, Row{
				Name:       r.Name,
				DurationUs: r.DurationUs,
				Score:      r.Score,
				CmpBase:    r.CmpBase,
				PrefIdx:    i, // attempt order
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
			dursUs[i] = ranking[i].DurationUs
		}

		klog.InfoS(label+": solver leaderboard",
			"ranking", names,
			"durationsUs", dursUs,
			"evictions", evs,
			"moves", mvs,
			"placedByPri", chosenSolverSummary.Score.PlacedByPriority)
	}

	// Build attempts for ledger
	evAttempts := make([]SolverStats, 0, len(results))
	for _, r := range results {
		evAttempts = append(evAttempts, SolverStats{
			Name:       r.Name,
			Status:     r.Status,
			DurationUs: r.DurationUs,
			Score:      r.Score,
		})
	}

	var evChosen *SolverStats
	if chosenOut != nil {
		evChosen = &SolverStats{
			Name:       chosenName,
			Status:     chosenStatus,
			DurationUs: chosenDurationUs,
			Score:      chosenScore,
		}
	}

	// Record stats if we had at least one feasible attempt.
	if anyFeasible {
		pl.appendStatsCM(ctx, ExportedStats{
			Timestamp_ns: time.Now().UnixNano(),
			Baseline:     baselineScore,
			Attempts:     evAttempts,
			Chosen:       evChosen,
			PlanStatus:   PlanStatusActive,
		})
	}

	return chosenOut, anyFeasible, chosenSolverSummary
}
