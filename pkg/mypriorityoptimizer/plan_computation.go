// plan_computation.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// planComputation tries enabled solvers in order, keeping the best feasible improvement.
// Returns: bestOut, whether anything was feasible, the best solver summary, and total duration (all attempts).
func (pl *SharedState) planComputation(
	ctx context.Context,
	solverInput SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
	baselineScore *SolverScore,
) (best string, hadFeasibleImprovingSolver bool, bestAttempt *SolverResult, attempts []SolverResult) {
	hadFeasibleImprovingSolver = false
	strategy := combinedModeToString()

	// Python tuning knobs live here, not inside SolverInput.
	pyOpts := PythonSolverOptions{
		LogProgress:            SolverLogProgress,
		GapLimit:               SolverPythonGapLimit,
		GuaranteedTierFraction: SolverPythonGuaranteedTierFraction,
		MoveFractionOfTier:     SolverPythonMoveFractionOfTier,
	}

	// =====================================
	// === Setup Attempts ==================
	// =====================================
	solverAttempts := []SolverAttempt{
		{
			Name:    "python",
			Enabled: SolverPythonEnabled,
			Timeout: SolverPythonTimeout + time.Duration(SolverPythonGraceMs)*time.Millisecond,
			Run: func(ctx context.Context, in SolverInput) (*SolverOutput, error) {
				return pl.runPythonSolver(ctx, in, pyOpts)
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
		inAttempt := solverInput
		inAttempt.TimeoutMs = att.Timeout.Milliseconds()

		// Build attempt context with timeout
		ctxDur := att.Timeout
		ctxAtt, cancel := context.WithTimeout(ctx, ctxDur)
		start := time.Now()
		out, err := att.Run(ctxAtt, inAttempt)
		cancel()
		durMs := time.Since(start).Milliseconds()

		if err != nil && out != nil { // error with output
			klog.InfoS(msg(strategy, InfoSolverFailed), "solver", att.Name, "status", out.Status, "err", err, "durationMs", durMs)
			attempts = append(attempts, SolverResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     out.Status,
				Score:      scoreSolution(inAttempt, out),
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
				Score:      scoreSolution(inAttempt, out),
			})
			// return the attempt as bestAttempt if no other attempts succeeded?
			if bestAttempt == nil {
				bestAttempt = &SolverResult{
					Name:       att.Name,
					DurationMs: durMs,
					Status:     out.Status,
					Score:      scoreSolution(inAttempt, out),
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
				Score:      scoreSolution(inAttempt, out),
			})
			continue
		}

		// Check if improving over baseline
		score := scoreSolution(inAttempt, out)
		improvedOverBaseline := isImprovement(*baselineScore, score)
		curr := SolverResult{
			Name:       att.Name,
			Output:     out,
			DurationMs: durMs,
			Score:      score,
			CmpBase:    improvedOverBaseline,
			Status:     out.Status,
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
