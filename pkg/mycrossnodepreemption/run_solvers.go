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
) (best SolverResult, hadFeasibleImprovingSolver bool) {
	hadFeasibleImprovingSolver = false
	strategy := strategyToString()

	// Baseline + prepared state
	baselineScore := buildBaselineScore(in)
	baseState := buildState(in)

	// Direct-fit pre-pass: only accept if strictly better than baseline
	if optimizeAtPreEnqueue() && optimizeEvery() {
		start := time.Now()
		if out := runSolverDirectFit(in, baseState); hasSolverFeasibleResult(out.Status) {
			score := computeSolverScore(in, out)
			best = SolverResult{
				Name:       "direct-fit",
				DurationUs: time.Since(start).Microseconds(),
				Score:      score,
				CmpBase:    1,
				Output:     out,
			}
			best.Status = out.Status
			klog.InfoS(msg(strategy, "direct-fit; skipping other solvers"),
				"placedByPri", best.Score.PlacedByPriority, "evictions", best.Score.Evicted, "moves", best.Score.Moved, "durationUs", best.DurationUs)
			return best, true
		}
		klog.V(MyV).InfoS(msg(strategy, "direct-fit could not place all pods; run solvers"), "durationUs", time.Since(start).Microseconds())
	}

	// =====================================
	// === Setup Attempts ==================
	// =====================================
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
	klog.V(MyV).InfoS(msg(strategy, "solver attempts planned"), "enabled", enabled)

	// Start with baseline as leader
	best = SolverResult{Name: "baseline", Score: baselineScore}

	// We will record only feasible+applicable attempts for the leaderboard/ledger
	var attemptsFeasibleImproving []SolverResult
	var attemptsFeasibleAll []SolverResult

	// =====================================
	// === Run Attempts ====================
	// =====================================
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

		if err != nil || out == nil {
			klog.InfoS(msg(strategy, InfoSolverFailed), "solver", att.Name, "err", err, "durationUs", durUs)
			attemptsFeasibleAll = append(attemptsFeasibleAll, SolverResult{Name: att.Name, DurationUs: durUs, Status: "FAILED"})
			continue
		} else if !hasSolverFeasibleResult(out.Status) {
			klog.InfoS(msg(strategy, InfoNoFeasibleOrOptimalSolution), "solver", att.Name, "status", out.Status, "stage", out.Stage, "durationUs", durUs)
			attemptsFeasibleAll = append(attemptsFeasibleAll, SolverResult{Name: att.Name, DurationUs: durUs, Status: out.Status, Stage: out.Stage})
			continue
		}
		// Check if plan
		ok, why := pl.planApplicable(out, nodes, pods)
		if !ok {
			klog.InfoS(msg(strategy, InfoPlanNotApplicable), "solver", att.Name, "reason", why, "durationUs", durUs)
			attemptsFeasibleAll = append(attemptsFeasibleAll, SolverResult{Name: att.Name, DurationUs: durUs, Status: "INFEASIBLE", Stage: "NOT-APPLICABLE"})
			continue
		}

		// Check if improving over baseline
		score := computeSolverScore(inAttempt, out)
		improvedOverBaseline := isImprovement(baselineScore, score)
		curr := SolverResult{
			Name:       att.Name,
			Output:     out,
			DurationUs: durUs,
			Score:      score,
			CmpBase:    improvedOverBaseline,
			Status:     out.Status,
			Stage:      out.Stage,
		}

		if improvedOverBaseline <= 0 {
			// Feasible but not improving (or no actual placement) — ignore as a candidate.
			klog.InfoS(
				msg(strategy, "feasible but not improving; discard"),
				"solver", att.Name,
				"placedByPri", score.PlacedByPriority,
				"baselinePlacedByPri", baselineScore.PlacedByPriority,
				"evictions", score.Evicted, "baselineEvictions", baselineScore.Evicted,
				"moves", score.Moved, "baselineMoves", baselineScore.Moved,
				"durationUs", durUs,
			)
			curr.CmpBase = 0 // mark as non-improving
			curr.Status = "FEASIBLE-NOT-IMPROVING"
			attemptsFeasibleAll = append(attemptsFeasibleAll, curr)
			continue
		}

		// From here on we have a strictly improving plan with actual placements
		hadFeasibleImprovingSolver = true
		attemptsFeasibleImproving = append(attemptsFeasibleImproving, curr)
		attemptsFeasibleAll = append(attemptsFeasibleAll, curr)

		// New leader?
		switch curr.CmpBase {
		case 1: // new best
			klog.V(MyV).InfoS(msg(strategy, "new leader"),
				"solver", att.Name, "prevLeader", best.Name, "durationUs", curr.DurationUs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", best.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", best.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", best.Score.Moved)
			best = curr // update leader
		case 0: // tie
			klog.V(MyV).InfoS(msg(strategy, "solver tied with leader"),
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
		default: // worse
			klog.V(MyV).InfoS(msg(strategy, "solver worse than leader"),
				"solver", att.Name, "leader", best.Name, "durationUs", curr.DurationUs,
				"placedByPri", curr.Score.PlacedByPriority, "leaderPlacedByPri", best.Score.PlacedByPriority,
				"evictions", curr.Score.Evicted, "leaderEvictions", best.Score.Evicted,
				"moves", curr.Score.Moved, "leaderMoves", best.Score.Moved)
		}
	}

	// =====================================
	// === Logging & Export ================
	// =====================================

	// Leaderboard
	logLeaderboard(strategy, attemptsFeasibleImproving, baselineScore, best)

	// Export solver attempts
	if SolverSaveAllAttempts {
		pl.exportSolverStats(ctx, strategy, baselineScore, best, attemptsFeasibleAll, hadFeasibleImprovingSolver)
	} else {
		pl.exportSolverStats(ctx, strategy, baselineScore, best, attemptsFeasibleImproving, hadFeasibleImprovingSolver)
	}

	return best, hadFeasibleImprovingSolver
}
