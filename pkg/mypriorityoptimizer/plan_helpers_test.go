package mypriorityoptimizer

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// tryEnterActive / leaveActive
// -----------------------------------------------------------------------------

func TestTryEnterActiveAndLeave(t *testing.T) {
	pl := &SharedState{}

	// First enter should succeed.
	if ok := pl.tryEnterActive(); !ok {
		t.Fatalf("first tryEnterActive() = false, want true")
	}

	// Second enter while still active should fail.
	if ok := pl.tryEnterActive(); ok {
		t.Fatalf("second tryEnterActive() = true, want false")
	}

	// After leaving, we should be able to enter again.
	pl.leaveActive()
	if ok := pl.tryEnterActive(); !ok {
		t.Fatalf("tryEnterActive() after leaveActive = false, want true")
	}
}

// -----------------------------------------------------------------------------
// getActivePlan / tryClearActivePlan
// -----------------------------------------------------------------------------

func TestGetAndClearActivePlan(t *testing.T) {
	pl := &SharedState{}

	// Initially no active plan.
	if ap := pl.getActivePlan(); ap != nil {
		t.Fatalf("getActivePlan() = %#v, want nil", ap)
	}

	ap := &ActivePlan{ID: "plan-1"}
	pl.ActivePlan.Store(ap)

	got := pl.getActivePlan()
	if got != ap {
		t.Fatalf("getActivePlan() = %#v, want %#v", got, ap)
	}

	// Clearing with the correct pointer should succeed.
	if ok := pl.tryClearActivePlan(ap); !ok {
		t.Fatalf("tryClearActivePlan(ap) = false, want true")
	}
	if got := pl.getActivePlan(); got != nil {
		t.Fatalf("getActivePlan() after clear = %#v, want nil", got)
	}

	// Clearing again (already nil) should fail.
	if ok := pl.tryClearActivePlan(ap); ok {
		t.Fatalf("tryClearActivePlan(ap) on nil active plan = true, want false")
	}

	// Clearing with nil should also fail.
	if ok := pl.tryClearActivePlan(nil); ok {
		t.Fatalf("tryClearActivePlan(nil) = true, want false")
	}
}

// -----------------------------------------------------------------------------
// buildPlan – nil SolverOutput
// -----------------------------------------------------------------------------

func TestBuildPlan_NilOutputReturnsEmptyPlan(t *testing.T) {
	pl := &SharedState{}

	plan, err := pl.buildPlan(nil, nil, nil)
	if err != nil {
		t.Fatalf("buildPlan(nil, nil, nil) error = %v, want nil", err)
	}
	if plan == nil {
		t.Fatalf("buildPlan(nil, nil, nil) = nil, want non-nil *Plan")
	}

	if len(plan.Evicts) != 0 {
		t.Errorf("plan.Evicts length = %d, want 0", len(plan.Evicts))
	}
	if len(plan.Moves) != 0 {
		t.Errorf("plan.Moves length = %d, want 0", len(plan.Moves))
	}
	if len(plan.OldPlacements) != 0 {
		t.Errorf("plan.OldPlacements length = %d, want 0", len(plan.OldPlacements))
	}
	if len(plan.NewPlacements) != 0 {
		t.Errorf("plan.NewPlacements length = %d, want 0", len(plan.NewPlacements))
	}
	if plan.PlacementByName != nil && len(plan.PlacementByName) != 0 {
		t.Errorf("plan.PlacementByName length = %d, want 0", len(plan.PlacementByName))
	}
	if plan.WorkloadQuotas != nil && len(plan.WorkloadQuotas) != 0 {
		t.Errorf("plan.WorkloadQuotas length = %d, want 0", len(plan.WorkloadQuotas))
	}
	if plan.NominatedNode != "" {
		t.Errorf("plan.NominatedNode = %q, want empty", plan.NominatedNode)
	}
}

// -----------------------------------------------------------------------------
// buildWorkloadQuotasAtomics
// -----------------------------------------------------------------------------

func TestBuildWorkloadQuotasAtomics_NilInput(t *testing.T) {
	got := buildWorkloadQuotasAtomics(nil)
	if got != nil {
		t.Fatalf("buildWorkloadQuotasAtomics(nil) = %#v, want nil", got)
	}
}

func TestBuildWorkloadQuotasAtomics_PopulatesAtomics(t *testing.T) {
	wkQuotas := WorkloadQuotas{
		"wk1": {
			"nodeA": 3,
			"nodeB": 0,  // non-positive; should still create a counter with value 0
		},
		"wk2": {
			"nodeC": -5, // non-positive; also should result in a counter with value 0
		},
	}

	got := buildWorkloadQuotasAtomics(wkQuotas)
	if got == nil {
		t.Fatalf("buildWorkloadQuotasAtomics(...) = nil, want non-nil map")
	}

	check := func(wk, node string, want int32) {
		perNode, ok := got[wk]
		if !ok {
			t.Fatalf("no entry for workload %q", wk)
		}
		ctr, ok := perNode[node]
		if !ok || ctr == nil {
			t.Fatalf("no counter for workload %q node %q", wk, node)
		}
		if have := ctr.Load(); have != want {
			t.Fatalf("counter[%q][%q] = %d, want %d", wk, node, have, want)
		}
	}

	check("wk1", "nodeA", 3)
	check("wk1", "nodeB", 0)
	check("wk2", "nodeC", 0)
}

// -----------------------------------------------------------------------------
// isPodAllowedByActivePlan – standalone / pinned-by-name behaviour
// -----------------------------------------------------------------------------

func TestIsPodAllowedByActivePlan_NoActivePlan(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "pod",
		},
	}
	if allowed := pl.isPodAllowedByActivePlan(pod); allowed {
		t.Fatalf("isPodAllowedByActivePlan with no active plan = true, want false")
	}
}

func TestIsPodAllowedByActivePlan_PinnedByName(t *testing.T) {
	pl := &SharedState{}

	ns, name := "ns", "pod"
	key := combineNsName(ns, name)

	ap := &ActivePlan{
		PlacementByName: map[string]string{
			key: "nodeA",
		},
	}
	pl.ActivePlan.Store(ap)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
	}

	if allowed := pl.isPodAllowedByActivePlan(pod); !allowed {
		t.Fatalf("isPodAllowedByActivePlan for pinned pod = false, want true")
	}
}

// -----------------------------------------------------------------------------
// filterNodes – basic paths (no active plan + standalone pinned)
// -----------------------------------------------------------------------------

func TestFilterNodes_NoActivePlan(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "pod",
		},
	}

	nodes, msg, ok := pl.filterNodes(pod)
	if !ok {
		t.Fatalf("filterNodes(no active plan) ok = false, want true")
	}
	if msg != InfoNoActivePlan {
		t.Fatalf("filterNodes message = %q, want %q", msg, InfoNoActivePlan)
	}
	if nodes != nil {
		t.Fatalf("filterNodes nodes = %#v, want nil when there is no active plan", nodes)
	}
}

func TestFilterNodes_PinnedStandalone(t *testing.T) {
	pl := &SharedState{}

	ns, name := "ns", "pod"
	key := combineNsName(ns, name)

	ap := &ActivePlan{
		PlacementByName: map[string]string{
			key: "nodeA",
		},
	}
	pl.ActivePlan.Store(ap)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
	}

	nodes, _, ok := pl.filterNodes(pod)
	if !ok {
		t.Fatalf("filterNodes for pinned pod ok = false, want true")
	}
	if nodes == nil || nodes.Len() != 1 || !nodes.Has("nodeA") {
		t.Fatalf("filterNodes allowed set = %#v, want {\"nodeA\"}", nodes)
	}
}
