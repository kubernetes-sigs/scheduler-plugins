package mypriorityoptimizer

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// runPythonSolver – wiring + JSON decode
// -----------------------------------------------------------------------------

func TestRunPythonSolver_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runPythonSolver test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
	}()

	tmpDir := t.TempDir()

	// Script: discard input and emit a plain SolverOutput JSON.
	script := `#!/usr/bin/env bash
cat >/dev/null
printf '{"status":"OPTIMAL","placements":[],"evictions":[]}'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runPythonSolver(ctx, PlannerInput{}, PythonSolverOptions{})
	if err != nil {
		t.Fatalf("runPythonSolver returned error: %v", err)
	}
	if out == nil {
		t.Fatalf("runPythonSolver returned nil output without error")
	}
	if out.Status != "OPTIMAL" {
		t.Fatalf("runPythonSolver output Status = %q, want %q", out.Status, "OPTIMAL")
	}
}

func TestRunPythonSolver_InvalidJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runPythonSolver test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
	}()

	tmpDir := t.TempDir()

	// Script: emit invalid JSON so that decode in runPythonSolver fails.
	script := `#!/usr/bin/env bash
cat >/dev/null
echo 'not-json'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runPythonSolver(ctx, PlannerInput{}, PythonSolverOptions{})
	if err == nil {
		t.Fatalf("expected error for invalid JSON output, got nil")
	}

	// Match the error message to ensure it's a decode error.
	if !strings.Contains(err.Error(), "decode") || !strings.Contains(err.Error(), "output") {
		t.Fatalf("expected decode error, got %v", err)
	}

	if out != nil {
		t.Fatalf("expected nil output on invalid JSON, got %#v", out)
	}
}
