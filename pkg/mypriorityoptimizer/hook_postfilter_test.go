// hook_postfilter_test.go
package mypriorityoptimizer

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

// -------------------------
// PostFilter
// --------------------------

// We only test the fast-path where PerPod@PostFilter is NOT enabled:
// PostFilter should return Unschedulable with a "no nomination" message.
func TestPostFilter_NoPerPod_NoNomination(t *testing.T) {

	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
		},
	}

	res, st := pl.PostFilter(context.Background(), nil, pod, nil)
	if res != nil {
		t.Fatalf("PostFilter() result = %#v, want nil when no nomination", res)
	}
	if st == nil {
		t.Fatalf("PostFilter() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
	if msg := st.Message(); !strings.Contains(msg, "PostFilter: no nomination") {
		t.Fatalf("PostFilter() message = %q, want to contain %q", msg, "PostFilter: no nomination")
	}
}

func TestPostFilter_PerPod_ActivePlanInProgress_BlocksPod(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePerPod, true, func() {
		res, st := pl.PostFilter(context.Background(), nil, pod, nil)
		if res != nil {
			t.Fatalf("PostFilter() result = %#v, want nil", res)
		}
		if st == nil {
			t.Fatalf("PostFilter() returned nil status")
		}
		if st.Code() != fwk.Unschedulable {
			t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
		}
		if got := pl.BlockedWhileActive.Size(); got != 1 {
			t.Fatalf("BlockedWhileActive.Size() = %d, want 1", got)
		}
	})
}

func TestPostFilter_PerPod_OptimizationReturnsErrActiveInProgress_BlocksPod(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.ActivePlanInProgress.Store(true) // force ErrActiveInProgress in runOptimizationFlow

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePerPod, true, func() {
		// Avoid the 1s sleep in tests.
		origSleep := postFilterSleep
		postFilterSleep = func(time.Duration) {}
		t.Cleanup(func() { postFilterSleep = origSleep })

		res, st := pl.PostFilter(context.Background(), nil, pod, nil)
		if res != nil {
			t.Fatalf("PostFilter() result = %#v, want nil", res)
		}
		if st == nil {
			t.Fatalf("PostFilter() returned nil status")
		}
		if st.Code() != fwk.Unschedulable {
			t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
		}
		if got := pl.BlockedWhileActive.Size(); got != 1 {
			t.Fatalf("BlockedWhileActive.Size() = %d, want 1", got)
		}
	})
}

func TestPostFilter_PerPod_OptimizationError_DefaultsToPlanRegistrationFailed(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePerPod, true, func() {
		origSleep := postFilterSleep
		postFilterSleep = func(time.Duration) {}
		t.Cleanup(func() { postFilterSleep = origSleep })

		// Force a generic error from runOptimizationFlow via planContextFn.
		origPlanContext := planContextFn
		planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
			return nil, nil, 0, SolverInput{}, context.Canceled
		}
		t.Cleanup(func() { planContextFn = origPlanContext })

		res, st := pl.PostFilter(context.Background(), nil, pod, nil)
		if res != nil {
			t.Fatalf("PostFilter() result = %#v, want nil", res)
		}
		if st == nil {
			t.Fatalf("PostFilter() returned nil status")
		}
		if st.Code() != fwk.Unschedulable {
			t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
		}
		if msg := st.Message(); !strings.Contains(msg, InfoPlanRegistrationFailed) {
			t.Fatalf("PostFilter() message = %q, want to contain %q", msg, InfoPlanRegistrationFailed)
		}
	})
}

func TestPostFilter_PerPod_OptimizationSuccess_ReturnsNomination(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePerPod, true, func() {
		origSleep := postFilterSleep
		postFilterSleep = func(time.Duration) {}
		t.Cleanup(func() { postFilterSleep = origSleep })

		origPlanContext := planContextFn
		origPlanComp := planComputationFn
		origApplicable := isSolutionApplicableFn
		origCounts := computePlanPodCountsFn
		origReg := planRegistrationFn
		origAct := planActivationFn
		origWatch := startPlanCompletionWatchFn
		origExport := exportSolverStatsFn

		planContextFn = func(_ *SharedState, _ *v1.Pod) ([]*v1.Node, []*v1.Pod, int, SolverInput, error) {
			inp := SolverInput{BaselineScore: SolverScore{}}
			return []*v1.Node{}, []*v1.Pod{}, 1, inp, nil
		}
		planComputationFn = func(_ *SharedState, _ context.Context, _ SolverInput) (string, bool, *SolverResult, *SolverOutput, []SolverResult) {
			best := &SolverResult{Name: "fake"}
			return "fake", true, best, &SolverOutput{}, []SolverResult{{Name: "fake"}}
		}
		isSolutionApplicableFn = func(_ *SharedState, _ *SolverOutput, _ []*v1.Node, _ []*v1.Pod) (bool, string) {
			return true, ""
		}
		computePlanPodCountsFn = func(_ *SolverOutput, _ []*v1.Pod) (int, int, int) {
			return 1, 1, 1
		}
		planRegistrationFn = func(_ *SharedState, _ context.Context, _ SolverResult, _ *SolverOutput, _ *v1.Pod, _ []*v1.Pod) (*Plan, *ActivePlan, error) {
			return &Plan{NominatedNode: "nodeA"}, &ActivePlan{ID: "ap1", PlacementByName: map[string]string{}}, nil
		}
		planActivationFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) error { return nil }
		startPlanCompletionWatchFn = func(_ *SharedState, _ *ActivePlan) {}
		exportSolverStatsFn = func(_ *SharedState, _ string, _ SolverScore, _ string, _ []SolverResult, _ string) {}

		t.Cleanup(func() {
			planContextFn = origPlanContext
			planComputationFn = origPlanComp
			isSolutionApplicableFn = origApplicable
			computePlanPodCountsFn = origCounts
			planRegistrationFn = origReg
			planActivationFn = origAct
			startPlanCompletionWatchFn = origWatch
			exportSolverStatsFn = origExport
		})

		res, st := pl.PostFilter(context.Background(), nil, pod, nil)
		if st == nil {
			t.Fatalf("PostFilter() returned nil status")
		}
		if st.Code() != fwk.Success {
			t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Success)
		}
		if res == nil || res.NominatingInfo == nil {
			t.Fatalf("PostFilter() result = %#v, want NominatingInfo", res)
		}
		if res.NominatingInfo.NominatedNodeName != "nodeA" {
			t.Fatalf("NominatedNodeName = %q, want %q", res.NominatingInfo.NominatedNodeName, "nodeA")
		}
	})
}
