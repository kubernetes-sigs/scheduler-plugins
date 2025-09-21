// run_solvers.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// TODO: still needs some cleanup...

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	in SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
) (best SolverAttemptResult, hadFeasibleSolver bool) {
	label := strategyToString()

	// Baseline + prepared state
	baselineScore := buildBaselineScore(in)
	baseState := buildState(in)

	// ---- Direct-fit pre-pass: only accept if strictly better than baseline ----
	if optimizeAtPreEnqueue() {
		start := time.Now()
		if out := runSolverDirectFit(in, baseState); HasSolverFeasibleResult(out.Status) {
			score := computeSolverScore(in, out)
			best = SolverAttemptResult{
				Name:       "direct-fit",
				DurationUs: time.Since(start).Microseconds(),
				Score:      score,
				CmpBase:    1,
				Output:     out,
			}
			klog.InfoS(label+": direct-fit; skipping other solvers",
				"placedByPri", best.Score.PlacedByPriority, "evictions", best.Score.Evicted, "moves", best.Score.Moved, "durationUs", best.DurationUs)
			return best, true
		}
		klog.V(MyV).InfoS(label+": direct-fit could not place all pods; run solvers", "durationUs", time.Since(start).Microseconds())
	}

	// Ordered attempts
	solverAttempts := []SolverAttempt{
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
			Run:     pl.runSolverPython,
		},
	}

	// Debug log
	enabled := []string{}
	for _, a := range solverAttempts {
		if a.Enabled {
			enabled = append(enabled, a.Name)
		}
	}
	klog.V(MyV).InfoS(label+": solver attempts planned", "enabled", enabled)

	// Start with baseline as leader
	best = SolverAttemptResult{Name: "baseline", Score: baselineScore}

	// We will record only feasible+applicable attempts for the leaderboard/ledger
	var attemptsFeasible []SolverAttemptResult

	// Loop over attempts
	for _, att := range solverAttempts {
		if !att.Enabled {
			continue
		}

		// Per-attempt input & hints
		inAttempt := in
		timeoutMs := att.Timeout.Milliseconds()
		if att.FudgeMs > 0 && timeoutMs > att.FudgeMs {
			timeoutMs -= att.FudgeMs
		}
		inAttempt.TimeoutMs = timeoutMs
		inAttempt.MaxTrials = att.Trials
		inAttempt.UseHints = SolverUseHints
		if SolverUseHints {
			inAttempt.Hints = cloneScore(best.Score)
		}

		// Run with timeout
		ctxAtt, cancel := context.WithTimeout(ctx, att.Timeout)
		start := time.Now()
		out, err := att.Run(ctxAtt, inAttempt)
		cancel()
		durUs := time.Since(start).Microseconds()

		if err != nil || !HasSolverFeasibleResult(out.Status) {
			klog.InfoS(label+": solver failed", "solver", att.Name, "status", out.Status, "durationUs", durUs)
			continue
		}
		ok, why := pl.planApplicable(out, nodes, pods)
		if !ok {
			klog.InfoS("Plan from solver is not applicable; skipping", "solver", att.Name, "reason", why, "durationUs", durUs)
			continue
		}

		// This attempt is feasible+applicable
		hadFeasibleSolver = true
		score := computeSolverScore(inAttempt, out)
		curr := SolverAttemptResult{
			Name:       att.Name,
			Output:     out,
			DurationUs: durUs,
			Score:      score,
			CmpBase:    IsImprovement(best.Score, score),
		}
		attemptsFeasible = append(attemptsFeasible, curr)

		// New leader?
		switch curr.CmpBase {
		case 1: // new best
			klog.V(MyV).InfoS(label+": new leader",
				"solver", att.Name, "prevLeader", best.Name, "durationUs", curr.DurationUs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", best.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", best.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", best.Score.Moved)
			best = curr
		case 0: // tie
			klog.V(MyV).InfoS(label+": solver tied with leader",
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
		default: // worse
			klog.V(MyV).InfoS(label+": solver worse than leader",
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "leaderPlacedByPri", best.Score.PlacedByPriority,
				"evictions", curr.Score.Evicted, "leaderEvictions", best.Score.Evicted,
				"moves", curr.Score.Moved, "leaderMoves", best.Score.Moved)
		}
	}

	// Upgrade OPTIMAL statuses if python is OPTIMAL and ties others
	pyIdx := -1
	for i := range attemptsFeasible {
		if attemptsFeasible[i].Name == "python" {
			pyIdx = i
			break
		}
	}
	if pyIdx >= 0 && attemptsFeasible[pyIdx].Output != nil && attemptsFeasible[pyIdx].Output.Status == "OPTIMAL" {
		pyScore := attemptsFeasible[pyIdx].Score
		ties := func(s SolverScore) bool {
			return IsImprovement(pyScore, s) == 0 && IsImprovement(s, pyScore) == 0
		}
		for i := range attemptsFeasible {
			if attemptsFeasible[i].Name != "python" && ties(attemptsFeasible[i].Score) && attemptsFeasible[i].Output != nil {
				attemptsFeasible[i].Output.Status = "OPTIMAL"
			}
		}
		if best.Name != "baseline" && best.Name != "python" && ties(best.Score) && best.Output != nil {
			best.Output.Status = "OPTIMAL"
		}
	}

	// Leaderboard
	if len(attemptsFeasible) > 0 {
		var better, equal, worse []SolverAttemptResult
		// Loop over feasible attempts to classify vs baseline
		for _, r := range attemptsFeasible {
			result := SolverAttemptResult{Name: r.Name, DurationUs: r.DurationUs, Score: r.Score, CmpBase: IsImprovement(baselineScore, r.Score)}
			switch result.CmpBase {
			case 1:
				better = append(better, result)
			case 0:
				equal = append(equal, result)
			default:
				worse = append(worse, result)
			}
		}
		// Build ranking: better first, then equal, then worse
		ranking := append(append(better, equal...), worse...)

		// Loop over ranking to build display arrays
		names := make([]string, len(ranking))
		evictions := make([]int, len(ranking))
		moves := make([]int, len(ranking))
		durations := make([]int64, len(ranking))
		for i := range ranking {
			label := ranking[i].Name
			if i > 0 &&
				IsImprovement(ranking[i-1].Score, ranking[i].Score) == 0 &&
				IsImprovement(ranking[i].Score, ranking[i-1].Score) == 0 {
				label += " (tie)"
			}
			names[i] = label
			evictions[i] = ranking[i].Score.Evicted
			moves[i] = ranking[i].Score.Moved
			durations[i] = ranking[i].DurationUs
		}

		// Placed by priority from baseline if not best
		placed := baselineScore.PlacedByPriority
		if best.Name != "baseline" {
			placed = best.Score.PlacedByPriority
		}
		// Show leaderboard
		klog.InfoS(label+": solver leaderboard",
			"ranking", names, "durationsUs", durations, "evictions", evictions, "moves", moves, "placedByPri", placed)
	}

	// Stats ledger
	if hadFeasibleSolver {
		attemptsToExport := make([]SolverSummary, 0, len(attemptsFeasible))
		for _, r := range attemptsFeasible {
			status := ""
			if r.Output != nil {
				status = r.Output.Status
			}
			attemptsToExport = append(attemptsToExport, SolverSummary{
				Name:       r.Name,
				Status:     status,
				DurationUs: r.DurationUs,
				Score:      r.Score,
			})
		}
		entry := ExportedStats{
			TimestampNs: time.Now().UnixNano(),
			Best:        best.Name,
			PlanStatus:  PlanStatusActive,
			Baseline:    baselineScore,
			Attempts:    attemptsToExport,
		}
		pl.appendStatsCM(ctx, entry)
	}

	return best, hadFeasibleSolver
}
