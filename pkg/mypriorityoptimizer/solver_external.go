// solver_external.go

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

	// test hook so we can force a read error from stdout
	readAllStdout = io.ReadAll
)

func (pl *SharedState) runSolverExternal(ctx context.Context, in SolverInput) (*SolverOutput, error) {
	rawInput, _ := json.Marshal(in)
	klog.V(MyV).InfoS("Solver input", "nodes", len(in.Nodes), "pods", len(in.Pods), "hasPreemptor", in.Preemptor != nil)

	cmd := execCommandContext(ctx, solverBinary, solverScriptPath)
	cmd.Stdin = bytes.NewReader(rawInput)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

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

	// --- this is the line we want to cover ---
	outBuf, err := readAllStdout(stdout)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read solver stdout: %w", err)
	}
	// -----------------------------------------

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf, &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	return &out, nil
}
