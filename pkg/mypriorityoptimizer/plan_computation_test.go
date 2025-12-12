// plan_computation_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"
	"time"
)

// -------------------------
// planComputation
// -------------------------

// No solvers enabled -> no attempts, no usable result, no best result.
func TestPlanComputation_NoEnabledSolvers(t *testing.T) {
	pl := &SharedState{}

	origPyEnabled := SolverPythonEnabled
	origHook := runPythonSolverHook
	defer func() {
		SolverPythonEnabled = origPyEnabled
		runPythonSolverHook = origHook
	}()

	SolverPythonEnabled = false
	runPythonSolverHook = nil

	in := SolverInput{
		BaselineScore: SolverScore{},
	}

	ctx := context.Background()
	bestName, hadUsable, bestAttempt, bestOutput, attempts := pl.planComputation(ctx, in)

	if hadUsable {
		t.Fatalf("hadUsableResult = true, want false when no solvers are enabled")
	}
	if bestName != "" {
		t.Fatalf("bestName = %q, want empty", bestName)
	}
	if bestAttempt != nil {
		t.Fatalf("bestAttempt = %#v, want nil", bestAttempt)
	}
	if bestOutput != nil {
		t.Fatalf("bestOutput = %#v, want nil", bestOutput)
	}
	if len(attempts) != 0 {
		t.Fatalf("len(attempts) = %d, want 0 when no solvers are enabled", len(attempts))
	}
}

// Solver enabled, but hook returns an error -> one FAILED attempt, no usable result.
func TestPlanComputation_SolverError(t *testing.T) {
	pl := &SharedState{}

	origPyEnabled := SolverPythonEnabled
	origTimeout := SolverPythonTimeout
	origGrace := SolverPythonGraceMs
	origHook := runPythonSolverHook
	defer func() {
		SolverPythonEnabled = origPyEnabled
		SolverPythonTimeout = origTimeout
		SolverPythonGraceMs = origGrace
		runPythonSolverHook = origHook
	}()

	SolverPythonEnabled = true
	SolverPythonTimeout = 10 * time.Millisecond
	SolverPythonGraceMs = 0

	runPythonSolverHook = func(_ *SharedState, _ context.Context, _ SolverInput, _ PythonSolverOptions) (*SolverOutput, error) {
		return nil, errors.New("boom")
	}

	in := SolverInput{
		BaselineScore: SolverScore{},
	}

	ctx := context.Background()
	bestName, hadUsable, bestAttempt, bestOutput, attempts := pl.planComputation(ctx, in)

	if hadUsable {
		t.Fatalf("hadUsableResult = true, want false on solver error")
	}
	if bestName != "" {
		t.Fatalf("bestName = %q, want empty on solver error", bestName)
	}
	if bestAttempt != nil {
		t.Fatalf("bestAttempt = %#v, want nil on solver error", bestAttempt)
	}
	if bestOutput != nil {
		t.Fatalf("bestOutput = %#v, want nil on solver error", bestOutput)
	}

	if len(attempts) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(attempts))
	}
	a := attempts[0]
	if a.Name != "python" {
		t.Fatalf("attempt Name = %q, want %q", a.Name, "python")
	}
	if a.Status != "FAILED" {
		t.Fatalf("attempt Status = %q, want %q", a.Status, "FAILED")
	}
}

// Solver enabled, hook returns OPTIMAL but score == baseline ->
// usable but NOT improving -> no best result, but attempt recorded.
func TestPlanComputation_UsableButNotImproving(t *testing.T) {
	pl := &SharedState{}

	origPyEnabled := SolverPythonEnabled
	origTimeout := SolverPythonTimeout
	origGrace := SolverPythonGraceMs
	origHook := runPythonSolverHook
	defer func() {
		SolverPythonEnabled = origPyEnabled
		SolverPythonTimeout = origTimeout
		SolverPythonGraceMs = origGrace
		runPythonSolverHook = origHook
	}()

	SolverPythonEnabled = true
	SolverPythonTimeout = 10 * time.Millisecond
	SolverPythonGraceMs = 0

	// Hook: just return an OPTIMAL plan with no placements/evictions.
	// scoreSolution(in, out) will be zero, equal to baseline.
	runPythonSolverHook = func(_ *SharedState, _ context.Context, _ SolverInput, _ PythonSolverOptions) (*SolverOutput, error) {
		return &SolverOutput{
			Status:     "OPTIMAL",
			Placements: nil,
			Evictions:  nil,
		}, nil
	}

	zero := SolverScore{}
	in := SolverInput{
		BaselineScore: zero,
		// No Pods / Preemptor -> scoreSolution also yields zero.
	}

	ctx := context.Background()
	bestName, hadUsable, bestAttempt, bestOutput, attempts := pl.planComputation(ctx, in)

	// Plan is usable but not strictly better than baseline.
	if hadUsable {
		t.Fatalf("hadUsableResult = true, want false when solution is usable but not improving")
	}
	if bestName != "" {
		t.Fatalf("bestName = %q, want empty when not improving", bestName)
	}
	if bestAttempt != nil {
		t.Fatalf("bestAttempt = %#v, want nil when not improving", bestAttempt)
	}
	if bestOutput != nil {
		t.Fatalf("bestOutput = %#v, want nil when not improving", bestOutput)
	}

	if len(attempts) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(attempts))
	}
	a := attempts[0]
	if a.Name != "python" {
		t.Fatalf("attempt Name = %q, want %q", a.Name, "python")
	}
	if a.Status != "OPTIMAL" {
		t.Fatalf("attempt Status = %q, want %q", a.Status, "OPTIMAL")
	}
	// Score should still be zero (equal to baseline).
	if a.Score.Evicted != 0 || a.Score.Moved != 0 || len(a.Score.PlacedByPriority) != 0 {
		t.Fatalf("attempt Score = %#v, want zero score", a.Score)
	}
}

// Solver enabled, hook returns OPTIMAL and strictly improves over baseline.
// Here we model a preemptor that gets placed.
func TestPlanComputation_UsableAndImproving(t *testing.T) {
	pl := &SharedState{}

	origPyEnabled := SolverPythonEnabled
	origTimeout := SolverPythonTimeout
	origGrace := SolverPythonGraceMs
	origHook := runPythonSolverHook
	defer func() {
		SolverPythonEnabled = origPyEnabled
		SolverPythonTimeout = origTimeout
		SolverPythonGraceMs = origGrace
		runPythonSolverHook = origHook
	}()

	SolverPythonEnabled = true
	SolverPythonTimeout = 10 * time.Millisecond
	SolverPythonGraceMs = 0

	pre := &SolverPod{
		UID:       "u-pre",
		Namespace: "ns",
		Name:      "pre",
		Priority:  5,
		Node:      "", // pending
	}

	// Hook: return a plan that places the preemptor on node n1.
	runPythonSolverHook = func(_ *SharedState, _ context.Context, _ SolverInput, _ PythonSolverOptions) (*SolverOutput, error) {
		return &SolverOutput{
			Status: "OPTIMAL",
			Placements: []SolverPod{
				{
					UID:       pre.UID,
					Namespace: pre.Namespace,
					Name:      pre.Name,
					Node:      "n1",
				},
			},
			Evictions: nil,
		}, nil
	}

	in := SolverInput{
		Preemptor:     pre,
		Pods:          nil,
		BaselineScore: SolverScore{}, // nothing placed initially
	}

	ctx := context.Background()
	bestName, hadUsable, bestAttempt, bestOutput, attempts := pl.planComputation(ctx, in)

	if !hadUsable {
		t.Fatalf("hadUsableResult = false, want true when solution is usable and improving")
	}
	if bestName != "python" {
		t.Fatalf("bestName = %q, want %q", bestName, "python")
	}
	if bestAttempt == nil {
		t.Fatalf("bestAttempt is nil, want non-nil")
	}
	if bestOutput == nil {
		t.Fatalf("bestOutput is nil, want non-nil")
	}

	if len(attempts) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(attempts))
	}
	a := attempts[0]
	if a.Name != "python" {
		t.Fatalf("attempt Name = %q, want %q", a.Name, "python")
	}
	if a.Status != "OPTIMAL" {
		t.Fatalf("attempt Status = %q, want %q", a.Status, "OPTIMAL")
	}
	if got := a.Score.PlacedByPriority["5"]; got != 1 {
		t.Fatalf("placedByPriority[\"5\"] = %d, want 1", got)
	}
	if a.Score.Evicted != 0 || a.Score.Moved != 0 {
		t.Fatalf("Evicted/Moved = (%d,%d), want (0,0)", a.Score.Evicted, a.Score.Moved)
	}

	// bestAttempt should match the only attempt
	if bestAttempt.Name != a.Name || bestAttempt.Status != a.Status {
		t.Fatalf("bestAttempt mismatch: got %#v, want %#v", bestAttempt, a)
	}
}
