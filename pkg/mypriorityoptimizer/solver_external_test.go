// solver_external_test.go
package mypriorityoptimizer

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// runSolverExternal
// -----------------------------------------------------------------------------

func TestRunSolverExternal_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	// Save & restore injected globals
	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
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
	solverBinary = "bash"
	solverScriptPath = scriptPath

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	in := SolverInput{} // contents not important for this test
	out, err := pl.runSolverExternal(ctx, in)
	if err != nil {
		t.Fatalf("runSolverExternal returned error: %v", err)
	}
	if out == nil {
		t.Fatalf("runSolverExternal returned nil output without error")
	}
	if out.Status != "OPTIMAL" {
		t.Fatalf("runSolverExternal output Status = %q, want %q", out.Status, "OPTIMAL")
	}
}

func TestRunSolverExternal_InvalidJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	// Save & restore injected globals
	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	// Script: consume stdin, then output invalid JSON
	script := `#!/usr/bin/env bash
cat >/dev/null
echo 'this-is-not-json'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)

	solverBinary = "bash"
	solverScriptPath = scriptPath

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	in := SolverInput{}
	out, err := pl.runSolverExternal(ctx, in)
	if err == nil {
		t.Fatalf("expected error for invalid JSON output, got nil")
	}
	if out != nil {
		t.Fatalf("expected nil output on invalid JSON, got %#v", out)
	}
}

// StdoutPipe error: simulate by starting the command before runSolverExternal
// calls StdoutPipe, which violates the os/exec contract.
func TestRunSolverExternal_StdoutPipeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	script := `#!/usr/bin/env bash
# trivial script
exit 0
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	// Wrap execCommandContext so the command is started before StdoutPipe().
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		_ = cmd.Start() // ignore error; we just want StdoutPipe() to fail later
		return cmd
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runSolverExternal(ctx, SolverInput{})
	if err == nil {
		t.Fatalf("expected error from StdoutPipe, got nil (out=%#v)", out)
	}
	if out != nil {
		t.Fatalf("expected nil output when StdoutPipe fails, got %#v", out)
	}
}

// StderrPipe error: simulate by pre-setting cmd.Stderr, so StderrPipe()
// returns an error ("exec: Stderr already set").
func TestRunSolverExternal_StderrPipeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	script := `#!/usr/bin/env bash
cat >/dev/null
echo '{"Status":"OPTIMAL"}'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		// Pre-assign Stderr so StderrPipe() will fail.
		cmd.Stderr = &bytes.Buffer{}
		return cmd
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runSolverExternal(ctx, SolverInput{})
	if err == nil {
		t.Fatalf("expected error from StderrPipe, got nil (out=%#v)", out)
	}
	if out != nil {
		t.Fatalf("expected nil output when StderrPipe fails, got %#v", out)
	}
}

// Start error: point solverBinary to a non-existent executable so Start() fails.
func TestRunSolverExternal_StartError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
	}()

	// Any name very unlikely to exist on PATH.
	solverBinary = "definitely-not-a-real-executable-xyz"
	solverScriptPath = "unused"

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runSolverExternal(ctx, SolverInput{})
	if err == nil {
		t.Fatalf("expected solver start error, got nil (out=%#v)", out)
	}
	if out != nil {
		t.Fatalf("expected nil output on start error, got %#v", out)
	}
}

// Wait error: script emits valid JSON but exits with non-zero status,
// so cmd.Wait() returns an error and we hit the "solver run: %w" path.
func TestRunSolverExternal_WaitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
	}()

	tmpDir := t.TempDir()

	script := `#!/usr/bin/env bash
cat >/dev/null
echo '{"Status":"OPTIMAL"}'
exit 3
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := pl.runSolverExternal(ctx, SolverInput{})
	if err == nil {
		t.Fatalf("expected error from solver run (non-zero exit), got nil")
	}
	if out != nil {
		t.Fatalf("expected nil output on solver run error, got %#v", out)
	}
}
