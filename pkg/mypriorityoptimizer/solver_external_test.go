// solver_external_test.go
package mypriorityoptimizer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// runSolverExternal – happy path
// -----------------------------------------------------------------------------

func TestRunSolverExternal_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	tmpDir := t.TempDir()

	// Script: consume stdin, then output some JSON-ish payload.
	script := `#!/usr/bin/env bash
# Consume all stdin (the solver input JSON)
cat >/dev/null
# Emit a minimal JSON payload
printf '{"status":"OPTIMAL"}'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	payload := []byte(`{"dummy":"input"}`)

	outBuf, err := pl.runSolverExternal(ctx, payload, "bash", scriptPath)
	if err != nil {
		t.Fatalf("runSolverExternal returned error: %v", err)
	}
	if outBuf == nil {
		t.Fatalf("runSolverExternal returned nil output without error")
	}

	got := strings.TrimSpace(string(outBuf))
	want := `{"status":"OPTIMAL"}`
	if got != want {
		t.Fatalf("runSolverExternal output = %q, want %q", got, want)
	}
}

// -----------------------------------------------------------------------------
// runSolverExternal – readAllStdout error
// -----------------------------------------------------------------------------

func TestRunSolverExternal_ReadStdoutError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	// Save & restore globals
	origBin := solverBinary
	origPath := solverScriptPath
	origExec := execCommandContext
	origRead := readAllStdout
	defer func() {
		solverBinary = origBin
		solverScriptPath = origPath
		execCommandContext = origExec
		readAllStdout = origRead
	}()

	tmpDir := t.TempDir()

	// Normal solver script (would normally succeed)
	script := `#!/usr/bin/env bash
cat >/dev/null
echo '{"Status":"OPTIMAL"}'
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)
	solverBinary = "bash"
	solverScriptPath = scriptPath

	// Force readAllStdout to fail so we hit the error path
	readAllStdout = func(r io.Reader) ([]byte, error) {
		return nil, fmt.Errorf("forced read error")
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payloadJson := []byte(`{"dummy":"data"}`)

	out, err := pl.runSolverExternal(ctx, payloadJson, SolverPythonBin, SolverPythonScriptPath)
	if err == nil {
		t.Fatalf("expected error from readAllStdout, got nil (out=%#v)", out)
	}
	if !strings.Contains(err.Error(), "read solver stdout") {
		t.Fatalf("expected read solver stdout error, got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil output on read error, got %#v", out)
	}
}

// -----------------------------------------------------------------------------
// runSolverExternal – StdoutPipe error
// -----------------------------------------------------------------------------

func TestRunSolverExternal_StdoutPipeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origExec := execCommandContext
	defer func() { execCommandContext = origExec }()

	// Force StdoutPipe() to fail by pre-setting cmd.Stdout.
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = &bytes.Buffer{}
		return cmd
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	outBuf, err := pl.runSolverExternal(ctx, []byte(`{}`), "bash", "/does/not/matter.sh")
	if err == nil {
		t.Fatalf("expected error from StdoutPipe, got nil (out=%q)", string(outBuf))
	}
	if outBuf != nil {
		t.Fatalf("expected nil output when StdoutPipe fails, got %q", string(outBuf))
	}
}

// -----------------------------------------------------------------------------
// runSolverExternal – StderrPipe error
// -----------------------------------------------------------------------------

func TestRunSolverExternal_StderrPipeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	origExec := execCommandContext
	defer func() { execCommandContext = origExec }()

	// Force StderrPipe() to fail by pre-setting cmd.Stderr.
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stderr = &bytes.Buffer{}
		return cmd
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	outBuf, err := pl.runSolverExternal(ctx, []byte(`{}`), "bash", "/does/not/matter.sh")
	if err == nil {
		t.Fatalf("expected error from StderrPipe, got nil (out=%q)", string(outBuf))
	}
	if outBuf != nil {
		t.Fatalf("expected nil output when StderrPipe fails, got %q", string(outBuf))
	}
}

// -----------------------------------------------------------------------------
// runSolverExternal – Start error
// -----------------------------------------------------------------------------

func TestRunSolverExternal_StartError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Use a command name that very likely does not exist on PATH, so Start() fails.
	outBuf, err := pl.runSolverExternal(ctx, []byte(`{}`),
		"definitely-not-a-real-executable-xyz", "unused")
	if err == nil {
		t.Fatalf("expected solver start error, got nil (out=%q)", string(outBuf))
	}
	if outBuf != nil {
		t.Fatalf("expected nil output on start error, got %q", string(outBuf))
	}
}

// -----------------------------------------------------------------------------
// runSolverExternal – Wait error (non-zero exit)
// -----------------------------------------------------------------------------

func TestRunSolverExternal_WaitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runSolverExternal test requires bash on PATH")
	}

	tmpDir := t.TempDir()

	// Script: emit valid JSON but exit with non-zero status.
	script := `#!/usr/bin/env bash
cat >/dev/null
printf '{"status":"OPTIMAL"}'
exit 3
`
	scriptPath := writeFakeSolverScript(t, tmpDir, script)

	pl := &SharedState{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	outBuf, err := pl.runSolverExternal(ctx, []byte(`{}`), "bash", scriptPath)
	if err == nil {
		t.Fatalf("expected error from solver run (non-zero exit), got nil")
	}
	if outBuf != nil {
		t.Fatalf("expected nil output on solver run error, got %q", string(outBuf))
	}
}
