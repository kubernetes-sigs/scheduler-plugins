package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// runPythonSolver
// -----------------------------------------------------------------------------

// runPythonSolver is the Python-specific wrapper that prepares the payload,
// invokes the external process, decodes the PythonSolverOutput and returns
// the embedded generic SolverOutput.
func (pl *SharedState) runPythonSolver(
	ctx context.Context,
	in SolverInput,
	opts PythonSolverOptions,
) (*SolverOutput, error) {
	// Prepare the payload
	payload := PythonSolverPayload{
		SolverInput:   in,
		SolverOptions: opts,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal python solver payload: %w", err)
	}

	// Solver-specific logging lives here (not in the generic helper).
	klog.V(MyV).InfoS("Python solver input",
		"nodes", len(in.Nodes),
		"pods", len(in.Pods),
		"hasPreemptor", in.Preemptor != nil,
		"timeoutMs", in.TimeoutMs,
		"logProgress", opts.LogProgress,
		"gapLimit", opts.GapLimit,
		"guaranteedTierFraction", opts.GuaranteedTierFraction,
		"moveFractionOfTier", opts.MoveFractionOfTier,
	)

	// Run the external solver process
	rawOut, err := pl.runSolverExternal(ctx, payloadJSON, solverBinary, solverScriptPath)
	if err != nil {
		return nil, fmt.Errorf("python solver external: %w", err)
	}

	// Decode the output – flat JSON: {"status": "...", "placements": [...], ...}
	var pyOut PythonSolverOutput
	if err := json.Unmarshal(rawOut, &pyOut); err != nil {
		return nil, fmt.Errorf("decode python solver output: %w", err)
	}

	out := &pyOut.SolverOutput

	// Summary log for the whole solve.
	klog.V(MyV).InfoS("Python solver result",
		"status", out.Status,
		"durationMs", out.DurationMs,
		"placements", len(out.Placements),
		"evictions", len(out.Evictions),
		"solvePhases", len(pyOut.SolvePhases),
	)

	// Per-phase progress log if present.
	for i, st := range pyOut.SolvePhases {
		klog.V(MyV).InfoS("Python solvePhase",
			"phase", i+1,
			"tier", st.Tier,
			"stage", st.Stage,
			"status", st.Status,
			"durationMs", st.DurationMs,
			"relativeGap", st.RelativeGap,
		)
	}

	return out, nil
}
