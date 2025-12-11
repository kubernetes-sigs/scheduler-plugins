// plan_computation.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"
)

// test hook: if non-nil, planComputation uses this instead of pl.runPythonSolver.
// In production this stays nil and the real solver is invoked.
var runPythonSolverHook func(
	pl *SharedState,
	ctx context.Context,
	in PlannerInput,
	opts PythonSolverOptions,
) (*PlannerOutput, error)

// planComputation tries enabled solvers in order, keeping the best attempt.
// Returns the name of the best solver, whether any usable result was found,
// the best attempt details, the best output, and all attempts.
func (pl *SharedState) planComputation(
	ctx context.Context,
	solverInput PlannerInput,
) (bestName string, hadUsableResult bool, bestAttempt *PlannerResult, bestOutput *PlannerOutput, attempts []PlannerResult) {
	hadUsableResult = false
	strategy := getModeCombinedAsString()

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
	solverAttempts := []PlannerAttempt{
		{
			Name:    "python",
			Enabled: SolverPythonEnabled,
			Timeout: SolverPythonTimeout + time.Duration(SolverPythonGraceMs)*time.Millisecond,
			Run: func(ctx context.Context, in PlannerInput) (*PlannerOutput, error) {
				// Use hook if present (unit tests); otherwise call real solver.
				if runPythonSolverHook != nil {
					return runPythonSolverHook(pl, ctx, in, pyOpts)
				}
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
	klog.InfoS(
		msg(strategy, "starting solvers"),
		"#pods", len(solverInput.Pods),
		"#nodes", len(solverInput.Nodes),
		"enabledSolvers", enabled,
	)

	// Moving current (strictly) best: start from baseline.
	currentBest := solverInput.BaselineScore

	// =====================================
	// === Run Attempts ====================
	// =====================================
	for _, att := range solverAttempts {
		if !att.Enabled {
			continue
		}

		inAttempt := solverInput
		inAttempt.TimeoutMs = att.Timeout.Milliseconds()

		ctxAtt, cancel := context.WithTimeout(ctx, att.Timeout)
		start := time.Now()
		out, err := att.Run(ctxAtt, inAttempt)
		cancel()
		durMs := time.Since(start).Milliseconds()

		// ---------- error / nil-output cases ----------
		if err != nil || out == nil {
			// Normalise error message + status
			errMsg := "nil output"
			if err != nil {
				errMsg = err.Error()
			}

			status := "FAILED"
			var score PlannerScore
			if out != nil {
				status = out.Status
				score = scorePlan(inAttempt, out)
			}

			klog.InfoS(
				msg(strategy, InfoSolverFailed),
				"solver", att.Name,
				"status", status,
				"err", errMsg,
				"durationMs", durMs,
			)

			attempts = append(attempts, PlannerResult{
				Name:       att.Name,
				DurationMs: durMs,
				Status:     status,
				Score:      score,
			})
			continue
		}

		// Build scored result
		res := PlannerResult{
			Name:       att.Name,
			DurationMs: durMs,
			Status:     out.Status,
			Score:      scorePlan(inAttempt, out),
		}

		// Is it a usable solution and does it improve over the current best?
		improvesCurrent, usable := doesSolverSolutionImproves(&currentBest, res.Status, &res.Score)
		if !usable {
			klog.InfoS(
				msg(strategy, InfoNoFeasibleOrOptimalSolution),
				"solver", att.Name,
				"status", out.Status,
				"durationMs", durMs,
			)
			attempts = append(attempts, res)
			continue
		}

		// Log also usable solutions
		attempts = append(attempts, res)

		if !improvesCurrent { // usable but not strictly better than the current best
			klog.InfoS(
				msg(strategy, fmt.Sprintf("%s but not improving current best", out.Status)),
				"solver", att.Name,
				"placedByPri", res.Score.PlacedByPriority,
				"baselinePlacedByPri", solverInput.BaselineScore.PlacedByPriority,
				"evictions", res.Score.Evicted, "baselineEvictions", solverInput.BaselineScore.Evicted,
				"moves", res.Score.Moved, "baselineMoves", solverInput.BaselineScore.Moved,
				"durationMs", durMs,
			)
			continue
		}

		// New leader
		hadUsableResult = true
		currentBest = res.Score
		bestAttempt = &res
		bestOutput = out
		bestName = res.Name
	}

	// Final log of best result
	logLeaderboard(strategy, attempts, solverInput.BaselineScore, bestAttempt)

	return bestName, hadUsableResult, bestAttempt, bestOutput, attempts
}
