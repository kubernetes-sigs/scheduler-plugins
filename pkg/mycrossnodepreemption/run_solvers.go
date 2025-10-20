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
			tmp := SolverResult{
				Name:       "direct-fit",
				DurationMs: time.Since(start).Milliseconds(),
				Score:      score,
				CmpBase:    isImprovement(*baselineScore, score),
				Output:     out,
				Status:     out.Status,
			}
			bestAttempt = &tmp
			klog.InfoS(msg(strategy, "direct-fit; skipping other solvers"),
				"placedByPri", score.PlacedByPriority, "evictions", score.Evicted, "moves", score.Moved, "durationMs", tmp.DurationMs)
			return bestAttempt.Name, bestAttempt.CmpBase > 0, bestAttempt, nil
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

	// =====================================
	// === Run Attempts ====================
	// =====================================
	bestAttempt = nil
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
				Status:     "FAILED",
			})
			// return the attempt as bestAttempt if no other attempts succeeded?
			if bestAttempt == nil {
				tmp := &SolverResult{
					Name:       att.Name,
					DurationMs: durMs,
					Status:     "FAILED",
				}
				bestAttempt = tmp
			}
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
			// return the attempt as bestAttempt if no other attempts succeeded?
			if bestAttempt == nil {
				bestAttempt = &SolverResult{
					Name:       att.Name,
					DurationMs: durMs,
					Status:     out.Status,
					Score:      computeSolverScore(inAttempt, out),
					Stages:     out.Stages,
				}
			}
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
			// Feasible but not improving (or no actual placement)
			klog.InfoS(
				msg(strategy, fmt.Sprintf("%s but not improving", out.Status)),
				"solver", att.Name,
				"placedByPri", score.PlacedByPriority,
				"baselinePlacedByPri", baselineScore.PlacedByPriority,
				"evictions", score.Evicted, "baselineEvictions", baselineScore.Evicted,
				"moves", score.Moved, "baselineMoves", baselineScore.Moved,
				"durationMs", durMs,
			)
			curr.CmpBase = 0 // explicitly non-improving vs baseline
			attempts = append(attempts, curr)

			// Ensure bestAttempt holds the best feasible/applicable solver attempt so far (even if not improving)
			if bestAttempt == nil {
				tmp := curr
				bestAttempt = &tmp
				klog.V(MyV).InfoS(msg(strategy, "first feasible candidate recorded as bestAttempt"),
					"solver", att.Name, "durationMs", curr.DurationMs,
					"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
			} else {
				// Compare curr vs bestAttempt using isImprovement(best, curr)
				cmp := isImprovement(bestAttempt.Score, curr.Score)
				switch {
				case cmp > 0:
					// curr better than bestAttempt
					klog.V(MyV).InfoS(msg(strategy, "new leader (non-improving tie-break)"),
						"solver", att.Name, "prevLeader", bestAttempt.Name, "durationMs", curr.DurationMs,
						"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", bestAttempt.Score.PlacedByPriority,
						"leaderEvictions", curr.Score.Evicted, "prevEvictions", bestAttempt.Score.Evicted,
						"leaderMoves", curr.Score.Moved, "prevMoves", bestAttempt.Score.Moved)
					tmp := curr
					bestAttempt = &tmp
				case cmp == 0:
					klog.V(MyV).InfoS(msg(strategy, "solver tied with bestAttempt (non-improving)"),
						"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
						"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
					// keep existing bestAttempt
				default: // cmp < 0 → curr worse than bestAttempt
					klog.V(MyV).InfoS(msg(strategy, "solver worse than bestAttempt (non-improving)"),
						"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
						"placedByPri", curr.Score.PlacedByPriority, "leaderPlacedByPri", bestAttempt.Score.PlacedByPriority,
						"evictions", curr.Score.Evicted, "leaderEvictions", bestAttempt.Score.Evicted,
						"moves", curr.Score.Moved, "leaderMoves", bestAttempt.Score.Moved)
				}
			}
			continue
		}

		// From here, we have a strictly improving plan with actual placements
		hadFeasibleImprovingSolver = true
		attempts = append(attempts, curr)

		if bestAttempt == nil {
			klog.V(MyV).InfoS(msg(strategy, "new leader (first improving)"),
				"solver", att.Name, "durationMs", curr.DurationMs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "leaderEvictions", curr.Score.Evicted, "leaderMoves", curr.Score.Moved)
			tmp := curr
			bestAttempt = &tmp
		} else if curr.CmpBase > bestAttempt.CmpBase { // better improvement class than bestAttempt
			klog.V(MyV).InfoS(msg(strategy, "new leader"),
				"solver", att.Name, "prevLeader", bestAttempt.Name, "durationMs", curr.DurationMs,
				"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", bestAttempt.Score.PlacedByPriority,
				"leaderEvictions", curr.Score.Evicted, "prevEvictions", bestAttempt.Score.Evicted,
				"leaderMoves", curr.Score.Moved, "prevMoves", bestAttempt.Score.Moved)
			tmp := curr
			bestAttempt = &tmp
		} else { // Same improvement class → tie-break with isImprovement(best, curr)
			cmp := isImprovement(bestAttempt.Score, curr.Score) // compare two solver scores
			if cmp > 0 {
				klog.V(MyV).InfoS(msg(strategy, "new leader"),
					"solver", att.Name, "prevLeader", bestAttempt.Name, "durationMs", curr.DurationMs,
					"leaderPlacedByPri", curr.Score.PlacedByPriority, "prevPlacedByPri", bestAttempt.Score.PlacedByPriority,
					"leaderEvictions", curr.Score.Evicted, "prevEvictions", bestAttempt.Score.Evicted,
					"leaderMoves", curr.Score.Moved, "prevMoves", bestAttempt.Score.Moved)
				tmp := curr
				bestAttempt = &tmp
			} else if cmp == 0 {
				klog.V(MyV).InfoS(msg(strategy, "solver tied with leader"),
					"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
					"placedByPri", curr.Score.PlacedByPriority, "evictions", curr.Score.Evicted, "moves", curr.Score.Moved)
			} else {
				klog.V(MyV).InfoS(msg(strategy, "solver worse than leader"),
					"solver", att.Name, "leader", bestAttempt.Name, "durationMs", curr.DurationMs,
					"placedByPri", curr.Score.PlacedByPriority, "leaderPlacedByPri", bestAttempt.Score.PlacedByPriority,
					"evictions", curr.Score.Evicted, "leaderEvictions", bestAttempt.Score.Evicted,
					"moves", curr.Score.Moved, "leaderMoves", bestAttempt.Score.Moved)
			}
		}
	}
	// Leaderboard
	if bestAttempt != nil {
		bestAttempt.Stages = nil
		logLeaderboard(strategy, attempts, *baselineScore, *bestAttempt)
	} else {
		logLeaderboard(strategy, attempts, *baselineScore, SolverResult{Name: "none"})
	}

	// Safe name return to avoid nil deref
	bestName := ""
	if bestAttempt != nil {
		bestName = bestAttempt.Name
	}
	return bestName, hadFeasibleImprovingSolver, bestAttempt, attempts
}
