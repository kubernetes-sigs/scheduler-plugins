// pkg/mypriorityoptimizer/plan_registration_test.go
// plan_registration_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
)

// -------------------------
// planRegistration – nil output
// -------------------------

func TestPlanRegistration_NilOutput(t *testing.T) {
	pl := &SharedState{}

	plan, ap, err := pl.planRegistration(
		context.Background(),
		SolverResult{},
		nil, // out is nil -> immediate error
		nil,
		nil,
	)

	if err == nil {
		t.Fatalf("planRegistration(nil output) error = nil, want non-nil")
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil when output is nil", plan)
	}
	if ap != nil {
		t.Fatalf("activePlan = %#v, want nil when output is nil", ap)
	}
}

// -------------------------
// planRegistration – buildPlan error
// -------------------------

func TestPlanRegistration_BuildPlanError(t *testing.T) {
	pl := &SharedState{}

	origBuild := buildPlanFn
	origExport := exportPlanToConfigMapFn
	defer func() {
		buildPlanFn = origBuild
		exportPlanToConfigMapFn = origExport
	}()

	// Make buildPlan fail.
	buildPlanFn = func(
		_ *SharedState,
		_ *SolverOutput,
		_ *v1.Pod,
		_ []*v1.Pod,
	) (*Plan, error) {
		return nil, errors.New("boom")
	}

	// If buildPlan fails, exportPlanToConfigMapFn must not be called.
	exportCalled := false
	exportPlanToConfigMapFn = func(
		_ *SharedState,
		_ context.Context,
		_ string,
		_ *StoredPlan,
	) error {
		exportCalled = true
		return nil
	}

	out := &SolverOutput{Status: "OPTIMAL"}

	plan, ap, err := pl.planRegistration(
		context.Background(),
		SolverResult{Name: "python"},
		out,
		nil,
		nil,
	)

	if err == nil {
		t.Fatalf("planRegistration() error = nil, want non-nil when buildPlan fails")
	}
	if got, wantSub := err.Error(), "build actions: boom"; got != wantSub {
		t.Fatalf("planRegistration() error = %q, want %q", got, wantSub)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil on buildPlan error", plan)
	}
	if ap != nil {
		t.Fatalf("activePlan = %#v, want nil on buildPlan error", ap)
	}
	if exportCalled {
		t.Fatalf("exportPlanToConfigMapFn was called, but should not be when buildPlan fails")
	}

	// Active plan should not have been set.
	if gotAP := pl.getActivePlan(); gotAP != nil {
		t.Fatalf("getActivePlan() = %#v, want nil after buildPlan error", gotAP)
	}
}

// -------------------------
// planRegistration – exportPlanToConfigMap error is ignored
// -------------------------

func TestPlanRegistration_ExportErrorIsIgnored(t *testing.T) {
	pl := &SharedState{}

	origBuild := buildPlanFn
	origExport := exportPlanToConfigMapFn
	defer func() {
		buildPlanFn = origBuild
		exportPlanToConfigMapFn = origExport
	}()

	// Stub buildPlan to return a simple plan.
	dummyPlan := &Plan{}
	buildPlanFn = func(
		_ *SharedState,
		_ *SolverOutput,
		_ *v1.Pod,
		_ []*v1.Pod,
	) (*Plan, error) {
		return dummyPlan, nil
	}

	// Capture what export sees, but make it fail.
	var (
		exportCalled bool
		gotID        string
		gotStored    *StoredPlan
	)
	exportPlanToConfigMapFn = func(
		_ *SharedState,
		_ context.Context,
		id string,
		stored *StoredPlan,
	) error {
		exportCalled = true
		gotID = id
		gotStored = stored
		return errors.New("cm write failed")
	}

	solverRes := SolverResult{
		Name:       "python",
		Status:     "OPTIMAL",
		DurationMs: 123,
		Score: SolverScore{
			PlacedByPriority: map[string]int{"5": 1},
			Evicted:          0,
			Moved:            0,
		},
	}

	out := &SolverOutput{
		Status: "OPTIMAL",
	}

	plan, ap, err := pl.planRegistration(
		context.Background(),
		solverRes,
		out,
		nil,
		nil,
	)

	// Export error must be logged but NOT propagated.
	if err != nil {
		t.Fatalf("planRegistration() error = %v, want nil when exportPlanToConfigMap fails", err)
	}
	if plan != dummyPlan {
		t.Fatalf("plan = %#v, want %#v (dummyPlan)", plan, dummyPlan)
	}
	if ap == nil {
		t.Fatalf("activePlan is nil, want non-nil")
	}

	// exportPlanToConfigMapFn must have been called.
	if !exportCalled {
		t.Fatalf("exportPlanToConfigMapFn was not called, want called once")
	}
	if gotID == "" {
		t.Fatalf("exportPlanToConfigMapFn got empty id, want non-empty")
	}
	if gotStored == nil {
		t.Fatalf("exportPlanToConfigMapFn storedPlan = nil, want non-nil")
	}

	// Basic invariants on StoredPlan.
	if gotStored.Plan != dummyPlan {
		t.Fatalf("stored.Plan = %#v, want %#v (dummyPlan)", gotStored.Plan, dummyPlan)
	}
	if gotStored.PlanStatus != PlanStatusActive {
		t.Fatalf("stored.PlanStatus = %v, want %v", gotStored.PlanStatus, PlanStatusActive)
	}
	if gotStored.PluginVersion != PluginVersion {
		t.Fatalf("stored.PluginVersion = %q, want %q", gotStored.PluginVersion, PluginVersion)
	}
	if gotStored.OptimizationStrategy != getModeCombinedAsString() {
		t.Fatalf("stored.OptimizationStrategy = %q, want %q",
			gotStored.OptimizationStrategy, getModeCombinedAsString())
	}
	if gotStored.GeneratedAt.IsZero() {
		t.Fatalf("stored.GeneratedAt is zero, want non-zero timestamp")
	}
	if gotStored.SolverResult.Name != solverRes.Name ||
		gotStored.SolverResult.Status != solverRes.Status {
		t.Fatalf("stored.SolverResult mismatch: got %+v, want %+v",
			gotStored.SolverResult, solverRes)
	}

	// Active plan should at least have the same ID used during export.
	if ap.ID != gotID {
		t.Fatalf("activePlan.ID = %q, want %q (same as stored plan id)", ap.ID, gotID)
	}
}
