package mypriorityoptimizer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// helper to write a temporary script that acts as the "python solver"
func writeFakeSolverScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake_solver.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("failed to write fake solver script: %v", err)
	}
	return path
}

func TestRunSolverPython_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverPython test requires bash on PATH")
	}

	// Save & restore injected globals
	origBin := solverPythonBin
	origPath := solverPath
	origExec := execCommandContext
	defer func() {
		solverPythonBin = origBin
		solverPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	// Script: consume stdin, then output valid SolverOutput JSON
	script := `#!/usr/bin/env bash
# Consume all stdin (the solver input JSON)
cat >/dev/null
# Emit a minimal valid SolverOutput JSON
echo '{"Status":"OPTIMAL"}'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)

	// For this test we call `bash <script>`, so:
	solverPythonBin = "bash"
	solverPath = scriptPath

	pl := &SharedState{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	in := SolverInput{} // contents not important for this test

	out, err := pl.runSolverPython(ctx, in)
	if err != nil {
		t.Fatalf("runSolverPython returned error: %v", err)
	}
	if out == nil {
		t.Fatalf("runSolverPython returned nil output without error")
	}
	if out.Status != "OPTIMAL" {
		t.Fatalf("runSolverPython output Status = %q, want %q", out.Status, "OPTIMAL")
	}
}

func TestRunSolverPython_InvalidJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverPython test requires bash on PATH")
	}

	// Save & restore injected globals
	origBin := solverPythonBin
	origPath := solverPath
	origExec := execCommandContext
	defer func() {
		solverPythonBin = origBin
		solverPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	// Script: consume stdin, then output invalid JSON
	script := `#!/usr/bin/env bash
cat >/dev/null
echo 'this-is-not-json'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)

	solverPythonBin = "bash"
	solverPath = scriptPath

	pl := &SharedState{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	in := SolverInput{}

	out, err := pl.runSolverPython(ctx, in)
	if err == nil {
		t.Fatalf("expected error for invalid JSON output, got nil")
	}
	if out != nil {
		t.Fatalf("expected nil output on invalid JSON, got %#v", out)
	}
}
