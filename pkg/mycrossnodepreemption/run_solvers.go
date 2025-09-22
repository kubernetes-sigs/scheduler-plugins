// run_solvers.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runSolvers tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *MyCrossNodePreemption) runSolvers(
	ctx context.Context,
	in SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
) (best SolverResult, hadFeasibleSolver bool) {
	strategy := strategyToString()

	// Baseline + prepared state
	baselineScore := buildBaselineScore(in)
	baseState := buildState(in)

	// Direct-fit pre-pass: only accept if strictly better than baseline
	if optimizeAtPreEnqueue() {
		start := time.Now()
		if out := runSolverDirectFit(in, baseState); HasSolverFeasibleResult(out.Status) {
			score := computeSolverScore(in, out)
			best = SolverResult{
				Name:       "direct-fit",
				DurationUs: time.Since(start).Microseconds(),
				Score:      score,
				CmpBase:    1,
				Output:     out,
			}
			best.Status = out.Status
			klog.InfoS(strategy+": direct-fit; skipping other solvers",
				"placedByPri", best.Score.PlacedByPriority, "evictions", best.Score.Evicted, "moves", best.Score.Moved, "durationUs", best.DurationUs)
			return best, true
		}
		klog.V(MyV).InfoS(strategy+": direct-fit could not place all pods; run solvers", "durationUs", time.Since(start).Microseconds())
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
	klog.V(MyV).InfoS(strategy+": solver attempts planned", "enabled", enabled)

	// Start with baseline as leader
	best = SolverResult{Name: "baseline", Score: baselineScore}

	// We will record only feasible+applicable attempts for the leaderboard/ledger
	var attemptsFeasible []SolverResult

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
			klog.InfoS(strategy+": solver failed", "solver", att.Name, "status", out.Status, "durationUs", durUs)
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
		curr := SolverResult{
			Name:       att.Name,
			Output:     out,
			DurationUs: durUs,
			Score:      score,
			CmpBase:    IsImprovement(best.Score, score),
			Status:     out.Status,
		}
		attemptsFeasible = append(attemptsFeasible, curr)

		// New leader?
		switch curr.CmpBase {
		case 1: // new best
			klog.V(MyV).InfoS(strategy+": new leader",
				"solver", att.Name, "prevLeader", best.Name, "durationUs", curr.DurationUs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", best.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", best.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", best.Score.Moved)
			best = curr
		case 0: // tie
			klog.V(MyV).InfoS(strategy+": solver tied with leader",
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
		default: // worse
			klog.V(MyV).InfoS(strategy+": solver worse than leader",
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
	pl.logLeaderboard(strategy, attemptsFeasible, baselineScore, best)

	// If any feasible solver, export stats
	pl.exportSolverStats(ctx, strategy, baselineScore, best, attemptsFeasible, hadFeasibleSolver)

	return best, hadFeasibleSolver
}
