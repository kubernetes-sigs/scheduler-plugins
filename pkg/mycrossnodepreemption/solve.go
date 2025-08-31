// solve.go
package mycrossnodepreemption

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

// TODO: Can we remove the in.Preemptor, and just use in.Pods?
func (pl *MyCrossNodePreemption) runSolver(ctx context.Context, in SolverInput) (*SolverOutput, error) {
	raw, _ := json.Marshal(in)
	klog.V(V2).InfoS("Solver input", "nodes", len(in.Nodes), "pods", len(in.Pods), "hasPreemptor", in.Preemptor != nil)

	cmd := exec.CommandContext(ctx, "python3", SolverPath)
	cmd.Stdin = bytes.NewReader(raw)

	// 1) Pipe stdout (JSON) and stderr (logs) separately
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	// 2) Stream stderr to klog line-by-line
	go func() {
		s := bufio.NewScanner(stderr)
		// bump buffer in case OR-Tools prints long lines
		buf := make([]byte, 0, 256*1024)
		s.Buffer(buf, 1024*1024)
		for s.Scan() {
			klog.V(V2).Info("solver: " + s.Text())
		}
		if err := s.Err(); err != nil {
			klog.Info("solver scan failed: " + err.Error())
		}
	}()

	// 3) Start and wait
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("solver start: %w", err)
	}

	// 4) Read full JSON from stdout
	outBuf, err := io.ReadAll(stdout)
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read solver stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		// Non‑zero exit; logs already streamed above
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf, &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if !IsSolverFeasible(&out) {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}
