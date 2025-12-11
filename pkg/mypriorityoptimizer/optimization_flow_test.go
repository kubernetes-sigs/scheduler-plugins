// optimization_flow_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
)

// -------------------------
// Helpers
// --------------------------

// dummySolverInput returns a minimal SolverInput with a specific baseline.
func dummySolverInput(evicted int) SolverInput {
	return SolverInput{
		BaselineScore: SolverScore{
			Evicted: evicted,
		},
	}
}

// -------------------------
// 1) Optimization gate: ErrOptimizationInProgress
// --------------------------

func TestRunOptimizationFlow_OptimizationInProgress(t *testing.T) {
	pl := &SharedState{}

	// First caller "owns" optimization.
	if !pl.tryEnterOptimizationFlow() {
		t.Fatalf("precondition: tryEnterOptimizationFlow() should succeed on fresh SharedState")
	}
	// Ensure we don't actually hit any heavy logic.
	origAsync := isAsyncSolvingFn
	isAsyncSolvingFn = func() bool { return false }
	defer func() { isAsyncSolvingFn = origAsync }()

	plan, baseline, bestName, bestAttempt, attempts, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrOptimizationInProgress) {
		t.Fatalf("err = %v, want ErrOptimizationInProgress", err)
	}
	if plan != nil || baseline != nil || bestName != "" || bestAttempt != nil || attempts != nil {
		t.Fatalf("expected all return values to be zero when optimization is already in progress")
	}
}

// -------------------------
// 2) Non-async: planContext error → leave PlanActive and propagate error
// --------------------------

func TestRunOptimizationFlow_PlanContextError(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		exportSolverStatsFn = origExportStats
	}()

	// Non-async mode → we take PlanActive early.
	isAsyncSolvingFn = func() bool { return false }

	// planContext fails.
	wantErr := errors.New("boom-plancontext")
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return nil, nil, 0, SolverInput{}, wantErr
	}

	// We should NOT export stats on this early failure.
	calledExport := atomic.Bool{}
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, _ string) {
		calledExport.Store(true)
	}

	plan, baseline, bestName, bestAttempt, attempts, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if plan != nil || baseline != nil || bestName != "" || bestAttempt != nil || attempts != nil {
		t.Fatalf("expected all return values to be zero when planContext fails")
	}
	if calledExport.Load() {
		t.Fatalf("exportSolverStatsFn should NOT be called on planContext error")
	}

	// Verify that PlanActive was released again (non-async path).
	if !pl.tryEnterActivePlan() {
		t.Fatalf("expected tryLeaveActivePlan to be called on planContext error")
	}
}

// -------------------------
// 3) Non-async: no improving solution → ErrNoImprovingSolutionFromAnySolver
// --------------------------

func TestRunOptimizationFlow_NoImprovingSolution(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		exportSolverStatsFn = origExportStats
	}()

	isAsyncSolvingFn = func() bool { return false }

	// Baseline score we expect to be threaded through.
	baseline := SolverScore{Evicted: 42}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return nil, nil, 3, dummySolverInput(baseline.Evicted), nil
	}

	// No improving solution from any solver.
	bestAttempt := &SolverResult{Name: "attempt-1"}
	attempts := []SolverResult{
		{Name: "solverA", Status: "FEASIBLE"},
		{Name: "solverB", Status: "OPTIMAL"},
	}
	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverB", false /* hadImprovement */, bestAttempt, nil, attempts
	}

	// Capture exported stats.
	calledExport := atomic.Bool{}
	var gotBaseline SolverScore
	var gotBestName string
	var gotErrMsg string

	exportSolverStatsFn = func(_ *SharedState, _ string, baselineScore SolverScore, bestName string, att []SolverResult, errMsg string) {
		calledExport.Store(true)
		gotBaseline = baselineScore
		gotBestName = bestName
		gotErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrNoImprovingSolutionFromAnySolver) {
		t.Fatalf("err = %v, want ErrNoImprovingSolutionFromAnySolver", err)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil", plan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverB" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverB")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt pointer mismatch")
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !calledExport.Load() {
		t.Fatalf("exportSolverStatsFn was not called on no-improvement path")
	}
	if gotBaseline.Evicted != baseline.Evicted {
		t.Fatalf("exported baseline = %#v, want %#v", gotBaseline, baseline)
	}
	if gotBestName != "solverB" {
		t.Fatalf("exported bestName = %q, want %q", gotBestName, "solverB")
	}
	if gotErrMsg != ErrNoImprovingSolutionFromAnySolver.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrNoImprovingSolutionFromAnySolver.Error())
	}

	// PlanActive should have been released.
	if !pl.tryEnterActivePlan() {
		t.Fatalf("expected tryLeaveActivePlan to be called on no-improvement path")
	}
}

// -------------------------
// 4) Non-async: plan not applicable → ErrPlanNotApplicable
// --------------------------

func TestRunOptimizationFlow_PlanNotApplicable(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		exportSolverStatsFn = origExportStats
	}()

	isAsyncSolvingFn = func() bool { return false }

	baseline := SolverScore{Evicted: 7}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return []*v1.Node{}, []*v1.Pod{}, 2, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-2"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverX"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverX", true, bestAttempt, bestOut, attempts
	}

	// Plan not applicable anymore.
	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return false, "stale-cluster"
	}

	calledExport := atomic.Bool{}
	var gotErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, errMsg string) {
		calledExport.Store(true)
		gotErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrPlanNotApplicable) {
		t.Fatalf("err = %v, want ErrPlanNotApplicable", err)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil", plan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverX" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverX")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt = %#v, want %#v", gotBestAttempt, bestAttempt)
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !calledExport.Load() {
		t.Fatalf("exportSolverStatsFn was not called on plan-not-applicable path")
	}
	if gotErrMsg != ErrPlanNotApplicable.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrPlanNotApplicable.Error())
	}

	// PlanActive should have been released.
	if !pl.tryEnterActivePlan() {
		t.Fatalf("expected tryLeaveActivePlan to be called on plan-not-applicable path")
	}
}

// -------------------------
// 5) Non-async: pendingScheduled == 0 → ErrNoPendingPodsScheduled
// --------------------------

func TestRunOptimizationFlow_NoPendingScheduled(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origComputeCounts := computePlanPodCountsFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		computePlanPodCountsFn = origComputeCounts
		exportSolverStatsFn = origExportStats
	}()

	isAsyncSolvingFn = func() bool { return false }

	baseline := SolverScore{Evicted: 10}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return []*v1.Node{}, []*v1.Pod{}, 2, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-3"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverY"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverY", true, bestAttempt, bestOut, attempts
	}

	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return true, ""
	}

	// No pending pods scheduled.
	computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
		return 0, 5, 5
	}

	calledExport := atomic.Bool{}
	var gotErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, errMsg string) {
		calledExport.Store(true)
		gotErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrNoPendingPodsScheduled) {
		t.Fatalf("err = %v, want ErrNoPendingPodsScheduled", err)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil", plan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverY" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverY")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt = %#v, want %#v", gotBestAttempt, bestAttempt)
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !calledExport.Load() {
		t.Fatalf("exportSolverStatsFn was not called on no-pending-scheduled path")
	}
	if gotErrMsg != ErrNoPendingPodsScheduled.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrNoPendingPodsScheduled.Error())
	}

	if !pl.tryEnterActivePlan() {
		t.Fatalf("expected tryLeaveActivePlan to be called on no-pending-scheduled path")
	}
}

// -------------------------
// 6) Non-async: happy path → success, watcher started, stats exported with no error
// --------------------------

func TestRunOptimizationFlow_SuccessfulPlan(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origComputeCounts := computePlanPodCountsFn
	origPlanReg := planRegistrationFn
	origPlanAct := planActivationFn
	origStartWatch := startPlanCompletionWatchFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		computePlanPodCountsFn = origComputeCounts
		planRegistrationFn = origPlanReg
		planActivationFn = origPlanAct
		startPlanCompletionWatchFn = origStartWatch
		exportSolverStatsFn = origExportStats
	}()

	isAsyncSolvingFn = func() bool { return false }

	baseline := SolverScore{Evicted: 0}
	nodes := []*v1.Node{}
	pods := []*v1.Pod{}
	pendingPrePlan := 2

	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return nodes, pods, pendingPrePlan, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-best"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverZ"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverZ", true, bestAttempt, bestOut, attempts
	}

	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return true, ""
	}

	// Some positive number of pendingScheduled.
	computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
		return 2, 5, 7
	}

	dummyPlan := &Plan{}
	dummyAP := &ActivePlan{ID: "plan-123"}

	planRegistrationFn = func(_ *SharedState, _ context.Context, _ SolverResult, _ *SolverOutput, _ *v1.Pod, _ []*v1.Pod) (*Plan, *ActivePlan, error) {
		return dummyPlan, dummyAP, nil
	}

	planActivationFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) error {
		return nil
	}

	watchCalled := atomic.Bool{}
	startPlanCompletionWatchFn = func(_ *SharedState, ap *ActivePlan) {
		if ap != dummyAP {
			t.Fatalf("startPlanCompletionWatchFn got ap=%#v, want %#v", ap, dummyAP)
		}
		watchCalled.Store(true)
	}

	exportCalled := atomic.Bool{}
	var exportErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, errMsg string) {
		exportCalled.Store(true)
		exportErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != dummyPlan {
		t.Fatalf("plan = %#v, want %#v", plan, dummyPlan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverZ" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverZ")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt = %#v, want %#v", gotBestAttempt, bestAttempt)
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !watchCalled.Load() {
		t.Fatalf("startPlanCompletionWatchFn was not called on success path")
	}
	if !exportCalled.Load() {
		t.Fatalf("exportSolverStatsFn was not called on success path")
	}
	if exportErrMsg != "" {
		t.Fatalf("exportSolverStatsFn errMsg = %q, want empty", exportErrMsg)
	}
}

// 7) Non-async: Active plan already in progress → ErrActiveInProgress
func TestRunOptimizationFlow_ActivePlanAlreadyInProgress_NonAsync(t *testing.T) {
	pl := &SharedState{}
	// Simulate an already-active plan.
	pl.ActivePlanInProgress.Store(true)

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		exportSolverStatsFn = origExportStats
	}()

	isAsyncSolvingFn = func() bool { return false }

	// If planContext is ever called, the test should fail → early return expected.
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		t.Fatalf("planContextFn must not be called when ActivePlan is already in progress")
		return nil, nil, 0, SolverInput{}, nil
	}

	// We should not export stats on this early ActivePlan conflict.
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, _ string) {
		t.Fatalf("exportSolverStatsFn must not be called on early ActivePlan conflict")
	}

	plan, baseline, bestName, bestAttempt, attempts, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrActiveInProgress) {
		t.Fatalf("err = %v, want ErrActiveInProgress", err)
	}
	if plan != nil || baseline != nil || bestName != "" || bestAttempt != nil || attempts != nil {
		t.Fatalf("expected all return values to be zero when ActivePlan is already in progress")
	}
}

// 8) Async: Active plan in progress at apply-time → ErrActiveInProgress
func TestRunOptimizationFlow_Async_ActivePlanInProgressAtApply(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origComputeCounts := computePlanPodCountsFn
	origPlanReg := planRegistrationFn
	origPlanAct := planActivationFn
	origStartWatch := startPlanCompletionWatchFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		computePlanPodCountsFn = origComputeCounts
		planRegistrationFn = origPlanReg
		planActivationFn = origPlanAct
		startPlanCompletionWatchFn = origStartWatch
		exportSolverStatsFn = origExportStats
	}()

	// Async mode: skip early PlanActive and only take it after plan is validated.
	isAsyncSolvingFn = func() bool { return true }

	baseline := SolverScore{Evicted: 11}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return []*v1.Node{}, []*v1.Pod{}, 1, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-async"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverAsync"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverAsync", true, bestAttempt, bestOut, attempts
	}

	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return true, ""
	}

	// pendingScheduled > 0 so we reach the async PlanActive branch.
	computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
		return 1, 4, 5
	}

	// Simulate that an Active plan is already in progress at apply-time.
	pl.ActivePlanInProgress.Store(true)

	// None of these should be reached if we fail at async tryEnterActivePlan.
	planRegistrationFn = func(_ *SharedState, _ context.Context, _ SolverResult, _ *SolverOutput, _ *v1.Pod, _ []*v1.Pod) (*Plan, *ActivePlan, error) {
		t.Fatalf("planRegistrationFn must not be called when async tryEnterActivePlan fails")
		return nil, nil, nil
	}
	planActivationFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) error {
		t.Fatalf("planActivationFn must not be called when async tryEnterActivePlan fails")
		return nil
	}
	startPlanCompletionWatchFn = func(_ *SharedState, _ *ActivePlan) {
		t.Fatalf("startPlanCompletionWatchFn must not be called when async tryEnterActivePlan fails")
	}

	exportCalled := atomic.Bool{}
	var gotBaseline SolverScore
	var gotBestName string
	var gotErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, baselineScore SolverScore, bestName string, _ []SolverResult, errMsg string) {
		exportCalled.Store(true)
		gotBaseline = baselineScore
		gotBestName = bestName
		gotErrMsg = errMsg
	}

	plan, baselinePtr, bestName, bestAttemptOut, attemptsOut, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrActiveInProgress) {
		t.Fatalf("err = %v, want ErrActiveInProgress", err)
	}
	// Async ErrActiveInProgress returns nils for everything.
	if plan != nil || baselinePtr != nil || bestName != "" || bestAttemptOut != nil || attemptsOut != nil {
		t.Fatalf("expected all return values to be zero on async ActiveInProgress")
	}

	if !exportCalled.Load() {
		t.Fatalf("exportSolverStatsFn must be called on async ActiveInProgress path")
	}
	if gotBaseline.Evicted != baseline.Evicted {
		t.Fatalf("exported baseline = %#v, want %#v", gotBaseline, baseline)
	}
	if gotBestName != "solverAsync" {
		t.Fatalf("exported bestName = %q, want %q", gotBestName, "solverAsync")
	}
	if gotErrMsg != ErrActiveInProgress.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrActiveInProgress.Error())
	}
}

// 9) Non-async: planRegistration fails → ErrPlanRegistration
func TestRunOptimizationFlow_PlanRegistrationError(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origComputeCounts := computePlanPodCountsFn
	origPlanReg := planRegistrationFn
	origPlanAct := planActivationFn
	origStartWatch := startPlanCompletionWatchFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		computePlanPodCountsFn = origComputeCounts
		planRegistrationFn = origPlanReg
		planActivationFn = origPlanAct
		startPlanCompletionWatchFn = origStartWatch
		exportSolverStatsFn = origExportStats
		onPlanCompletedHook = nil
	}()

	isAsyncSolvingFn = func() bool { return false }

	baseline := SolverScore{Evicted: 5}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return []*v1.Node{}, []*v1.Pod{}, 3, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-regerr"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverReg"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverReg", true, bestAttempt, bestOut, attempts
	}

	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return true, ""
	}

	computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
		return 2, 4, 6
	}

	wantErr := errors.New("boom-planreg")
	planRegistrationFn = func(_ *SharedState, _ context.Context, _ SolverResult, _ *SolverOutput, _ *v1.Pod, _ []*v1.Pod) (*Plan, *ActivePlan, error) {
		return nil, nil, wantErr
	}

	// PlanActivation should not be reached.
	planActivationFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) error {
		t.Fatalf("planActivationFn must not be called when planRegistration fails")
		return nil
	}
	startPlanCompletionWatchFn = func(_ *SharedState, _ *ActivePlan) {
		t.Fatalf("startPlanCompletionWatchFn must not be called when planRegistration fails")
	}

	// Only check stats export here.
	exportCalled := atomic.Bool{}
	var gotErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, errMsg string) {
		exportCalled.Store(true)
		gotErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrPlanRegistration) {
		t.Fatalf("err = %v, want ErrPlanRegistration", err)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil", plan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverReg" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverReg")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt = %#v, want %#v", gotBestAttempt, bestAttempt)
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !exportCalled.Load() {
		t.Fatalf("exportSolverStatsFn must be called on planRegistration error")
	}
	if gotErrMsg != ErrPlanRegistration.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrPlanRegistration.Error())
	}
}

// 10) Non-async: planActivation fails → ErrPlanActivationFailed and onPlanCompleted
func TestRunOptimizationFlow_PlanActivationError(t *testing.T) {
	pl := &SharedState{}

	origAsync := isAsyncSolvingFn
	origPlanCtx := planContextFn
	origPlanComp := planComputationFn
	origIsApplicable := isSolutionApplicableFn
	origComputeCounts := computePlanPodCountsFn
	origPlanReg := planRegistrationFn
	origPlanAct := planActivationFn
	origStartWatch := startPlanCompletionWatchFn
	origExportStats := exportSolverStatsFn
	defer func() {
		isAsyncSolvingFn = origAsync
		planContextFn = origPlanCtx
		planComputationFn = origPlanComp
		isSolutionApplicableFn = origIsApplicable
		computePlanPodCountsFn = origComputeCounts
		planRegistrationFn = origPlanReg
		planActivationFn = origPlanAct
		startPlanCompletionWatchFn = origStartWatch
		exportSolverStatsFn = origExportStats
		onPlanCompletedHook = nil
	}()

	isAsyncSolvingFn = func() bool { return false }

	baseline := SolverScore{Evicted: 2}
	planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
		return []*v1.Node{}, []*v1.Pod{}, 1, dummySolverInput(baseline.Evicted), nil
	}

	bestAttempt := &SolverResult{Name: "attempt-acterr"}
	bestOut := &SolverOutput{Status: "OPTIMAL"}
	attempts := []SolverResult{{Name: "solverAct"}}

	planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
		return "solverAct", true, bestAttempt, bestOut, attempts
	}

	isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
		return true, ""
	}

	computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
		return 1, 3, 4
	}

	dummyPlan := &Plan{}
	dummyAP := &ActivePlan{ID: "plan-activation"}

	// Simulate that planRegistration would have set an active plan.
	pl.ActivePlan.Store(dummyAP)

	planRegistrationFn = func(_ *SharedState, _ context.Context, _ SolverResult, _ *SolverOutput, _ *v1.Pod, _ []*v1.Pod) (*Plan, *ActivePlan, error) {
		return dummyPlan, dummyAP, nil
	}

	wantErr := errors.New("boom-activation")
	planActivationFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) error {
		return wantErr
	}

	// We should not start watcher if planActivation fails.
	startPlanCompletionWatchFn = func(_ *SharedState, _ *ActivePlan) {
		t.Fatalf("startPlanCompletionWatchFn must not be called when planActivation fails")
	}

	planCompleted := atomic.Bool{}
	var completedStatus PlanStatus
	onPlanCompletedHook = func(_ *SharedState, status PlanStatus, _ *ActivePlan) {
		planCompleted.Store(true)
		completedStatus = status
	}

	exportCalled := atomic.Bool{}
	var gotErrMsg string
	exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, errMsg string) {
		exportCalled.Store(true)
		gotErrMsg = errMsg
	}

	plan, bPtr, bestName, gotBestAttempt, gotAttempts2, err :=
		pl.runOptimizationFlow(context.Background(), nil)

	if !errors.Is(err, ErrPlanActivationFailed) {
		t.Fatalf("err = %v, want ErrPlanActivationFailed", err)
	}
	if plan != nil {
		t.Fatalf("plan = %#v, want nil", plan)
	}
	if bPtr == nil || (*bPtr).Evicted != baseline.Evicted {
		t.Fatalf("baseline = %#v, want %#v", bPtr, baseline)
	}
	if bestName != "solverAct" {
		t.Fatalf("bestName = %q, want %q", bestName, "solverAct")
	}
	if gotBestAttempt != bestAttempt {
		t.Fatalf("bestAttempt = %#v, want %#v", gotBestAttempt, bestAttempt)
	}
	if len(gotAttempts2) != len(attempts) {
		t.Fatalf("attempts length = %d, want %d", len(gotAttempts2), len(attempts))
	}

	if !planCompleted.Load() {
		t.Fatalf("onPlanCompletedHook must be called when planActivation fails and an ActivePlan exists")
	}
	if completedStatus != PlanStatusFailed {
		t.Fatalf("onPlanCompletedHook status = %v, want %v", completedStatus, PlanStatusFailed)
	}

	if !exportCalled.Load() {
		t.Fatalf("exportSolverStatsFn must be called on planActivation error")
	}
	if gotErrMsg != ErrPlanActivationFailed.Error() {
		t.Fatalf("exported errMsg = %q, want %q", gotErrMsg, ErrPlanActivationFailed.Error())
	}
}
