// solver_external.go
package mypriorityoptimizer

import (
	"bufio"
	"bytes"
	"context"
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

// runSolverExternal is the generic external solver runner.
func (pl *SharedState) runSolverExternal(
	ctx context.Context,
	payload []byte,
	binary string,
	scriptPath string,
) ([]byte, error) {
	// Prepare the command
	cmd := execCommandContext(ctx, binary, scriptPath)

	// Provide payload via stdin
	cmd.Stdin = bytes.NewReader(payload)

	// Capture stdout for reading
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Capture stderr for logging
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	// Stream solver logs from stderr
	go func() {
		s := bufio.NewScanner(stderr)
		buf := make([]byte, 0, 256*1024) // 256KB initial buffer
		s.Buffer(buf, 1024*1024)         // 1MB max token size
		for s.Scan() {
			klog.V(MyV).Info("solver: " + s.Text())
		}
		if err := s.Err(); err != nil {
			klog.Info("solver scan failed: " + err.Error())
		}
	}()

	// Start the solver process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("solver start: %w", err)
	}

	// Read all stdout (blocking until EOF)
	outBuf, err := readAllStdout(stdout)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read solver stdout: %w", err)
	}

	// Wait for the solver to exit
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("solver run: %w", err)
	}

	return outBuf, nil
}
