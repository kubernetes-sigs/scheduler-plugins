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

	for _, att := range solverAttempts {
		if !att.Enabled {
			continue
		}

		// Per-attempt input & hints
		inAttempt := in
		toMs := att.Timeout.Milliseconds()
		if att.FudgeMs > 0 && toMs > att.FudgeMs {
			toMs -= att.FudgeMs
		}
		inAttempt.TimeoutMs = toMs
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

		switch curr.CmpBase {
		case 1:
			klog.V(MyV).InfoS(label+": new leader",
				"solver", att.Name, "prevLeader", best.Name, "durationUs", curr.DurationUs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", best.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", best.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", best.Score.Moved)
			best = curr
		case 0:
			klog.V(MyV).InfoS(label+": solver tied with leader",
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
		default:
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

	// Leaderboard log (use best if improved; otherwise baseline)
	if len(attemptsFeasible) > 0 {
		// Partition by improvement vs baseline: better → equal → worse
		type row struct {
			Name       string
			DurationUs int64
			Score      SolverScore
			CmpBase    int
		}
		var better, equal, worse []row
		for _, r := range attemptsFeasible {
			r2 := row{r.Name, r.DurationUs, r.Score, IsImprovement(baselineScore, r.Score)}
			switch r2.CmpBase {
			case 1:
				better = append(better, r2)
			case 0:
				equal = append(equal, r2)
			default:
				worse = append(worse, r2)
			}
		}
		ranking := append(append(better, equal...), worse...)

		names := make([]string, len(ranking))
		evs := make([]int, len(ranking))
		mvs := make([]int, len(ranking))
		durs := make([]int64, len(ranking))
		for i := range ranking {
			lbl := ranking[i].Name
			if i > 0 &&
				IsImprovement(ranking[i-1].Score, ranking[i].Score) == 0 &&
				IsImprovement(ranking[i].Score, ranking[i-1].Score) == 0 {
				lbl += " (tie)"
			}
			names[i] = lbl
			evs[i] = ranking[i].Score.Evicted
			mvs[i] = ranking[i].Score.Moved
			durs[i] = ranking[i].DurationUs
		}

		placed := baselineScore.PlacedByPriority
		if best.Name != "baseline" {
			placed = best.Score.PlacedByPriority
		}
		klog.InfoS(label+": solver leaderboard",
			"ranking", names, "durationsUs", durs, "evictions", evs, "moves", mvs, "placedByPri", placed)
	}

	// ---- Stats ledger ----
	if hadFeasibleSolver {
		evAttempts := make([]SolverSummary, 0, len(attemptsFeasible))
		for _, r := range attemptsFeasible {
			status := ""
			if r.Output != nil {
				status = r.Output.Status
			}
			evAttempts = append(evAttempts, SolverSummary{
				Name:       r.Name,
				Status:     status,
				DurationUs: r.DurationUs,
				Score:      r.Score,
			})
		}
		entry := ExportedStats{
			Timestamp_ns: time.Now().UnixNano(),
			Baseline:     baselineScore,
			Attempts:     evAttempts,
		}
		// Mark chosen only if improved
		if best.Name != "baseline" && best.Output != nil {
			entry.Chosen = &SolverSummary{
				Name:       best.Name,
				Status:     best.Output.Status,
				DurationUs: best.DurationUs,
				Score:      best.Score,
			}
			entry.PlanStatus = PlanStatusActive
		}
		pl.appendStatsCM(ctx, entry)
	}

	return best, hadFeasibleSolver
}
