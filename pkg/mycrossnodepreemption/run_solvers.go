// run_solvers.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
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
	baselineScore *SolverScore,
) (best string, hadFeasibleImprovingSolver bool, bestAttempt *SolverResult, attempts []SolverResult) {
	hadFeasibleImprovingSolver = false
	strategy := strategyToString()

	// Prepared state
	baseState := buildState(in)

	// Direct-fit pre-pass: only accept if strictly better than baseline
	if optimizeAtPreEnqueue() && optimizeEvery() {
		start := time.Now()
		if out := runSolverDirectFit(in, baseState); hasSolverFeasibleResult(out.Status) {
			score := computeSolverScore(in, out)
			bestAttempt = &SolverResult{
				Name:       "direct-fit",
				DurationMs: time.Since(start).Milliseconds(),
				Score:      score,
				CmpBase:    1,
				Output:     out,
				Status:     out.Status,
			}
			klog.InfoS(msg(strategy, "direct-fit; skipping other solvers"),
				"placedByPri", bestAttempt.Score.PlacedByPriority, "evictions", bestAttempt.Score.Evicted, "moves", bestAttempt.Score.Moved, "durationMs", bestAttempt.DurationMs)
			return bestAttempt.Name, true, bestAttempt, nil
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
			Run: func(ctx context.Context, in SolverInput) (*SolverOutput, error) {
				return pl.runSolverPython(ctx, in)
			},
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
	bestAttempt = &SolverResult{Name: "baseline", Score: *baselineScore}

	// =====================================
	// === Run Attempts ====================
	// =====================================
	for _, att := range solverAttempts {
		if !att.Enabled {
			continue
		}

		// Per-attempt input & hints
		inAttempt := in
		inAttempt.TimeoutMs = att.Timeout.Milliseconds() // ← no grace added here
		inAttempt.MaxTrials = att.Trials

		// Build attempt context with timeout + grace
		ctxDur := att.Timeout

		// Python specific parameters
		if att.Name == "python" {
			inAttempt.GapLimit = SolverPythonGapLimit
			inAttempt.GuaranteedTierFraction = SolverPythonGuaranteedTierFraction
			inAttempt.MoveFractionOfTier = SolverPythonMoveFractionOfTier
			if SolverPythonGraceMs > 0 { // increase context duration by grace for python due to I/O overhead
				ctxDur += time.Duration(SolverPythonGraceMs) * time.Millisecond
			}
		}

		ctxAtt, cancel := context.WithTimeout(ctx, ctxDur)
		start := time.Now()
		out, err := att.Run(ctxAtt, inAttempt) // grace is ignored by non-python
		cancel()
		durMs := time.Since(start).Milliseconds()

		if err != nil && out != nil { // error with output
			klog.InfoS(msg(strategy, InfoSolverFailed), "solver", att.Name, "status", out.Status, "err", err, "durationMs", durMs)
			attempts = append(attempts, SolverResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     out.Status,
				Score:      computeSolverScore(inAttempt, out),
				Stages:     out.Stages,
			})
			continue
		} else if err != nil { // error with no output
			klog.InfoS(msg(strategy, InfoSolverFailed), "solver", att.Name, "err", err, "durationMs", durMs)
			attempts = append(attempts, SolverResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     "failed-no-output",
			})
			continue
		} else if !hasSolverFeasibleResult(out.Status) { // no feasible or optimal solution
			klog.InfoS(msg(strategy, InfoNoFeasibleOrOptimalSolution), "solver", att.Name, "status", out.Status, "durationMs", durMs)
			attempts = append(attempts, SolverResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     out.Status,
				Score:      computeSolverScore(inAttempt, out),
				Stages:     out.Stages,
			})
			continue
		}

		// Check if plan is actually applicable on the current cluster state
		ok, why := pl.planApplicable(out, nodes, pods)
		if !ok {
			klog.InfoS(msg(strategy, InfoPlanNotApplicable), "solver", att.Name, "status", out.Status, "reason", why, "durationMs", durMs)
			attempts = append(attempts, SolverResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     out.Status,
				Score:      computeSolverScore(inAttempt, out),
				Stages:     out.Stages,
			})
			continue
		}

		// Check if improving over baseline
		score := computeSolverScore(inAttempt, out)
		improvedOverBaseline := isImprovement(*baselineScore, score)
		curr := SolverResult{
			Name:       att.Name,
			Output:     out,
			DurationMs: durMs,
			Score:      score,
			CmpBase:    improvedOverBaseline,
			Status:     out.Status,
			Stages:     out.Stages,
		}

		if improvedOverBaseline <= 0 {
			// Feasible but not improving (or no actual placement) — ignore as a candidate.
			klog.InfoS(
				msg(strategy, fmt.Sprintf("%s but not improving; discard", out.Status)),
				"solver", att.Name,
				"placedByPri", score.PlacedByPriority,
				"baselinePlacedByPri", baselineScore.PlacedByPriority,
				"evictions", score.Evicted, "baselineEvictions", baselineScore.Evicted,
				"moves", score.Moved, "baselineMoves", baselineScore.Moved,
				"durationMs", durMs,
			)
			curr.CmpBase = 0 // mark as non-improving
			attempts = append(attempts, curr)
			continue
		}

		// From here, we have a strictly improving plan with actual placements
		hadFeasibleImprovingSolver = true
		attempts = append(attempts, curr)

		// New leader?
		switch curr.CmpBase {
		case 1:
			klog.V(MyV).InfoS(msg(strategy, "new leader"),
				"solver", att.Name, "prevLeader", bestAttempt.Name, "durationMs", curr.DurationMs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", bestAttempt.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", bestAttempt.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", bestAttempt.Score.Moved)
			bestAttempt = &curr // update leader
		case 0: // tie
			klog.V(MyV).InfoS(msg(strategy, "solver tied with leader"),
				"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
				"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
		default: // worse
			klog.V(MyV).InfoS(msg(strategy, "solver worse than leader"),
				"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
				"placedByPri", curr.Score.PlacedByPriority, "leaderPlacedByPri", bestAttempt.Score.PlacedByPriority,
				"evictions", curr.Score.Evicted, "leaderEvictions", bestAttempt.Score.Evicted,
				"moves", curr.Score.Moved, "leaderMoves", bestAttempt.Score.Moved)
		}
	}

	// Leaderboard
	bestAttempt.Stages = nil
	logLeaderboard(strategy, attempts, *baselineScore, *bestAttempt)

	return bestAttempt.Name, hadFeasibleImprovingSolver, bestAttempt, attempts
}
