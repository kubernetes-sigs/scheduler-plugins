// pkg/mypriorityoptimizer/solver_external.go
package mypriorityoptimizer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"k8s.io/klog/v2"
)

var (
	execCommandContext = exec.CommandContext
	solverBinary       = SolverPythonBin
	solverScriptPath   = SolverPythonScriptPath
	readAllStdout      = io.ReadAll
)

// runPythonSolver is the Python-specific wrapper that prepares the payload,
// invokes the external process, decodes the PythonSolverOutput and returns
// the embedded generic SolverOutput.
func (pl *SharedState) runPythonSolver(
	ctx context.Context,
	in SolverInput,
	opts PythonSolverOptions,
) (*SolverOutput, error) {
	// Build payload: nested solver_input + python_solver_options.
	payload := PythonSolverPayload{
		SolverInput:         in,
		PythonSolverOptions: opts,
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

	rawOut, err := pl.runSolverExternal(ctx, payloadJSON, solverBinary, solverScriptPath)
	if err != nil {
		return nil, fmt.Errorf("python solver external: %w", err)
	}

	// Decode rich Python-specific output (generic fields + stages).
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
		"stages", len(pyOut.Stages),
	)

	// Per-stage progress log if present.
	for _, st := range pyOut.Stages {
		klog.V(MyV).InfoS("Python solver stage",
			"tier", st.Tier,
			"stage", st.Stage,
			"status", st.Status,
			"durationMs", st.DurationMs,
			"relativeGap", st.RelativeGap,
		)
	}

	return out, nil
}

// runSolverExternal is the generic "spawn a process, send JSON, get JSON" helper.
func (pl *SharedState) runSolverExternal(
	ctx context.Context,
	payload []byte,
	binary string,
	scriptPath string,
) ([]byte, error) {
	cmd := execCommandContext(ctx, binary, scriptPath)
	cmd.Stdin = bytes.NewReader(payload)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	// Stream solver logs from stderr.
	go func() {
		s := bufio.NewScanner(stderr)
		buf := make([]byte, 0, 256*1024)
		s.Buffer(buf, 1024*1024)
		for s.Scan() {
			klog.V(MyV).Info("solver: " + s.Text())
		}
		if err := s.Err(); err != nil {
			klog.Info("solver scan failed: " + err.Error())
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("solver start: %w", err)
	}

	outBuf, err := readAllStdout(stdout)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read solver stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("solver run: %w", err)
	}
	return outBuf, nil
}
