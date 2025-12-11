// plan_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// ----------------------------------------------------------------------
// tryEnterActive / leaveActive / getActivePlan / tryClearActivePlan
// ----------------------------------------------------------------------

func TestTryEnterActive_AndLeaveActive(t *testing.T) {
	pl := &SharedState{}

	// First enter should succeed and flip Active to true.
	if ok := pl.tryEnterActivePlan(); !ok {
		t.Fatalf("first tryEnterActive() = %v, want true", ok)
	}
	if got := pl.ActivePlanInProgress.Load(); !got {
		t.Fatalf("Active.Load() = %v, want true after first enter", got)
	}

	// Second enter should fail (already active).
	if ok := pl.tryEnterActivePlan(); ok {
		t.Fatalf("second tryEnterActive() = %v, want false", ok)
	}

	// Leaving should reset the flag.
	pl.tryLeaveActivePlan()
	if got := pl.ActivePlanInProgress.Load(); got {
		t.Fatalf("Active.Load() = %v, want false after leaveActive()", got)
	}
}

func TestGetAndClearActivePlan(t *testing.T) {
	pl := &SharedState{}
	ap1 := &ActivePlan{ID: "p1"}
	ap2 := &ActivePlan{ID: "p2"}

	// No plan initially.
	if got := pl.getActivePlan(); got != nil {
		t.Fatalf("getActivePlan() = %v, want nil", got)
	}

	// Store a plan and get it back.
	pl.ActivePlan.Store(ap1)
	if got := pl.getActivePlan(); got != ap1 {
		t.Fatalf("getActivePlan() = %p, want %p", got, ap1)
	}

	// tryClear with wrong pointer should fail.
	if ok := pl.tryClearActivePlan(ap2); ok {
		t.Fatalf("tryClearActivePlan(ap2) = %v, want false", ok)
	}
	if got := pl.getActivePlan(); got != ap1 {
		t.Fatalf("after failed clear, plan = %p, want %p", got, ap1)
	}

	// tryClear with correct pointer should succeed and nil out.
	if ok := pl.tryClearActivePlan(ap1); !ok {
		t.Fatalf("tryClearActivePlan(ap1) = %v, want true", ok)
	}
	if got := pl.getActivePlan(); got != nil {
		t.Fatalf("after successful clear, plan = %p, want nil", got)
	}

	// tryClearActivePlan(nil) should just return false.
	if ok := pl.tryClearActivePlan(nil); ok {
		t.Fatalf("tryClearActivePlan(nil) = %v, want false", ok)
	}
}

// ----------------------------------------------------------------------
// toPlanPod
// ----------------------------------------------------------------------

func TestToPlanPod_BasicConversion(t *testing.T) {
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "pod1",
			UID:       types.UID("uid-123"),
		},
		Spec: v1.PodSpec{
			NodeName: "nodeA",
		},
	}
	pp := toPlanPod(p)
	want := SolverPod{
		UID:       p.UID,
		Namespace: p.Namespace,
		Name:      p.Name,
	}
	if !reflect.DeepEqual(pp, want) {
		t.Fatalf("toPlanPod() = %+v, want %+v", pp, want)
	}
}

// ----------------------------------------------------------------------
// increaseWorkloadQuota
// ----------------------------------------------------------------------

func TestIncreaseWorkloadQuota_NewAndExisting(t *testing.T) {
	wq := WorkloadQuotas{}
	wk := WorkloadKey{} // zero value is fine, we only care that wk.String() is stable

	// First increment should allocate the inner map and set to 1.
	increaseWorkloadQuota(wq, wk, "node-a")
	key := wk.String()
	if wq[key] == nil {
		t.Fatalf("wq[%q] is nil, want non-nil map", key)
	}
	if got := wq[key]["node-a"]; got != 1 {
		t.Fatalf("wq[%q][node-a] = %d, want 1", key, got)
	}

	// Second increment on the same key/node should be 2.
	increaseWorkloadQuota(wq, wk, "node-a")
	if got := wq[key]["node-a"]; got != 2 {
		t.Fatalf("wq[%q][node-a] = %d, want 2", key, got)
	}

	// Different node should get its own counter.
	increaseWorkloadQuota(wq, wk, "node-b")
	if got := wq[key]["node-b"]; got != 1 {
		t.Fatalf("wq[%q][node-b] = %d, want 1", key, got)
	}
}

// ----------------------------------------------------------------------
// sortPlacementsByPod
// ----------------------------------------------------------------------

func TestSortPlacementsByPod_SortsByNamespaceThenName(t *testing.T) {
	in := []SolverPod{
		{Namespace: "ns-b", Name: "x"},
		{Namespace: "ns-a", Name: "z"},
		{Namespace: "ns-a", Name: "a"},
	}
	sortPlacementsByPod(in)

	got := []string{
		in[0].Namespace + "/" + in[0].Name,
		in[1].Namespace + "/" + in[1].Name,
		in[2].Namespace + "/" + in[2].Name,
	}
	want := []string{"ns-a/a", "ns-a/z", "ns-b/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted = %#v, want %#v", got, want)
	}
}

// ----------------------------------------------------------------------
// sortNewPlacementsByPod
// ----------------------------------------------------------------------

func TestSortNewPlacementsByPod_SortsByNamespaceThenName(t *testing.T) {
	in := []SolverPod{
		{Namespace: "ns-b", Name: "x"},
		{Namespace: "ns-a", Name: "z"},
		{Namespace: "ns-a", Name: "a"},
	}
	sortNewPlacementsByPod(in)

	got := []string{
		in[0].Namespace + "/" + in[0].Name,
		in[1].Namespace + "/" + in[1].Name,
		in[2].Namespace + "/" + in[2].Name,
	}
	want := []string{"ns-a/a", "ns-a/z", "ns-b/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted = %#v, want %#v", got, want)
	}
}

// ----------------------------------------------------------------------
// sortPodSetItemsByPriorityAndCreation
// ----------------------------------------------------------------------

func TestSortPodSetItemsByPriorityAndCreation_PriorityDominates(t *testing.T) {
	var pLowPrio int32 = 1
	var pHighPrio int32 = 10

	now := metav1.Now()
	older := metav1.NewTime(now.Add(-time.Minute))

	pLow := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "low",
			CreationTimestamp: older,
		},
		Spec: v1.PodSpec{Priority: &pLowPrio},
	}
	pHigh := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "high",
			CreationTimestamp: now,
		},
		Spec: v1.PodSpec{Priority: &pHighPrio},
	}

	items := []SafePodSetItem{
		{p: pLow},
		{p: pHigh},
	}
	sortPodSetItemsByPriorityAndCreation(items)

	if items[0].p.Name != "high" || items[1].p.Name != "low" {
		t.Fatalf("order = %s,%s, want high,low", items[0].p.Name, items[1].p.Name)
	}
}

func TestSortPodSetItemsByPriorityAndCreation_TimestampThenName(t *testing.T) {
	var prio int32 = 5

	now := metav1.Now()
	older := metav1.NewTime(now.Add(-time.Minute))

	pOld := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "old",
			CreationTimestamp: older,
		},
		Spec: v1.PodSpec{Priority: &prio},
	}
	pNew := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "new",
			CreationTimestamp: now,
		},
		Spec: v1.PodSpec{Priority: &prio},
	}

	items := []SafePodSetItem{
		{p: pNew},
		{p: pOld},
	}
	sortPodSetItemsByPriorityAndCreation(items)

	if items[0].p.Name != "old" || items[1].p.Name != "new" {
		t.Fatalf("order = %s,%s, want old,new", items[0].p.Name, items[1].p.Name)
	}
}

func TestSortPodSetItemsByPriorityAndCreation_NameFallbackOnZeroTimestamp(t *testing.T) {
	var prio int32 = 5

	zeroTS := metav1.Time{} // IsZero() == true
	pA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "a",
			CreationTimestamp: zeroTS,
		},
		Spec: v1.PodSpec{Priority: &prio},
	}
	pC := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "c",
			CreationTimestamp: zeroTS,
		},
		Spec: v1.PodSpec{Priority: &prio},
	}

	items := []SafePodSetItem{
		{p: pC},
		{p: pA},
	}
	sortPodSetItemsByPriorityAndCreation(items)

	if items[0].p.Name != "a" || items[1].p.Name != "c" {
		t.Fatalf("order = %s,%s, want a,c (name fallback)", items[0].p.Name, items[1].p.Name)
	}
}

// ----------------------------------------------------------------------
// buildPlan
// ----------------------------------------------------------------------

func TestBuildPlan_NilOutputReturnsEmptyPlan(t *testing.T) {
	pl := &SharedState{}

	plan, err := pl.buildPlan(nil, nil, nil)
	if err != nil {
		t.Fatalf("buildPlan(nil, ...) error = %v, want nil", err)
	}
	if plan == nil {
		t.Fatalf("buildPlan(nil, ...) = nil, want non-nil empty plan")
	}
	if len(plan.Evicts) != 0 || len(plan.Moves) != 0 || len(plan.NewPlacements) != 0 {
		t.Fatalf("empty plan not empty: %+v", plan)
	}
}

func TestBuildPlan_BasicEvictsMovesAndPending(t *testing.T) {
	pl := &SharedState{}

	// Pods:
	//  - p1: running on n1, will be evicted
	//  - p2: running on n1, moved to n2
	//  - p3: pending, placed on n1
	p1 := newPod("ns", "p1", "u1", "n1", "", "", 1)
	p2 := newPod("ns", "p2", "u2", "n1", "", "", 1)
	p3 := newPod("ns", "p3", "u3", "", "", "", 1)

	out := &SolverOutput{
		Evictions: []SolverPod{
			{UID: p1.UID},
		},
		Placements: []SolverPod{
			{UID: p2.UID, Node: "n2"},
			{UID: p3.UID, Node: "n1"},
		},
	}

	plan, err := pl.buildPlan(out, nil, []*v1.Pod{p1, p2, p3})
	if err != nil {
		t.Fatalf("buildPlan() error = %v, want nil", err)
	}
	if plan == nil {
		t.Fatalf("buildPlan() = nil, want non-nil")
	}

	// Old placements: p1 and p2 (sorted by name)
	if got := len(plan.OldPlacements); got != 2 {
		t.Fatalf("OldPlacements len = %d, want 2", got)
	}
	if plan.OldPlacements[0].Name != "p1" || plan.OldPlacements[0].Node != "n1" {
		t.Fatalf("OldPlacements[0] = %#v, want p1@n1", plan.OldPlacements[0])
	}
	if plan.OldPlacements[1].Name != "p2" || plan.OldPlacements[1].Node != "n1" {
		t.Fatalf("OldPlacements[1] = %#v, want p2@n1", plan.OldPlacements[1])
	}

	// Evicts: only p1 from n1.
	if got := len(plan.Evicts); got != 1 {
		t.Fatalf("Evicts len = %d, want 1", got)
	}
	if plan.Evicts[0].Name != "p1" || plan.Evicts[0].Node != "n1" {
		t.Fatalf("Evicts[0] = %#v, want p1@n1", plan.Evicts[0])
	}

	// New placements: p2 (moved n1->n2), p3 (pending->n1), sorted by name.
	if got := len(plan.NewPlacements); got != 2 {
		t.Fatalf("NewPlacements len = %d, want 2", got)
	}
	if plan.NewPlacements[0].Name != "p2" {
		t.Fatalf("NewPlacements[0].Name = %q, want %q", plan.NewPlacements[0].Name, "p2")
	}
	if plan.NewPlacements[0].OldNode != "n1" || plan.NewPlacements[0].Node != "n2" {
		t.Fatalf("NewPlacements[0] = %#v, want From=n1 To=n2", plan.NewPlacements[0])
	}
	if plan.NewPlacements[1].Name != "p3" {
		t.Fatalf("NewPlacements[1].Name = %q, want %q", plan.NewPlacements[1].Name, "p3")
	}
	if plan.NewPlacements[1].OldNode != "" || plan.NewPlacements[1].Node != "n1" {
		t.Fatalf("NewPlacements[1] = %#v, want From=\"\" To=n1", plan.NewPlacements[1])
	}

	// Moves: only p2 (moved between nodes).
	if got := len(plan.Moves); got != 1 {
		t.Fatalf("Moves len = %d, want 1", got)
	}
	if plan.Moves[0].Name != "p2" || plan.Moves[0].OldNode != "n1" || plan.Moves[0].Node != "n2" {
		t.Fatalf("Moves[0] = %#v, want p2 From=n1 To=n2", plan.Moves[0])
	}

	// placementByName should contain standalone pods p2 and p3.
	want2 := mergeNsName("ns", "p2")
	want3 := mergeNsName("ns", "p3")
	if got := plan.PlacementByName[want2]; got != "n2" {
		t.Fatalf("PlacementByName[%q] = %q, want %q", want2, got, "n2")
	}
	if got := plan.PlacementByName[want3]; got != "n1" {
		t.Fatalf("PlacementByName[%q] = %q, want %q", want3, got, "n1")
	}

	// No preemptor -> NominatedNode should be empty.
	if plan.NominatedNode != "" {
		t.Fatalf("NominatedNode = %q, want \"\"", plan.NominatedNode)
	}
}

func TestBuildPlan_WithPreemptorNomination(t *testing.T) {
	pl := &SharedState{}

	pre := newPod("ns", "pre", "pre-uid", "", "", "", 1)

	out := &SolverOutput{
		Placements: []SolverPod{
			{UID: pre.UID, Node: "n-pre"},
		},
	}

	plan, err := pl.buildPlan(out, pre, []*v1.Pod{pre})
	if err != nil {
		t.Fatalf("buildPlan() error = %v, want nil", err)
	}
	if plan == nil {
		t.Fatalf("buildPlan() = nil, want non-nil")
	}

	// Preemptor should set NominatedNode and be in PlacementByName.
	if plan.NominatedNode != "n-pre" {
		t.Fatalf("NominatedNode = %q, want %q", plan.NominatedNode, "n-pre")
	}
	key := mergeNsName("ns", "pre")
	if got := plan.PlacementByName[key]; got != "n-pre" {
		t.Fatalf("PlacementByName[%q] = %q, want %q", key, got, "n-pre")
	}

	// Preemptor placement should NOT show up in Moves (we `continue` after nomination).
	if len(plan.Moves) != 0 {
		t.Fatalf("Moves len = %d, want 0 for preemptor-only plan", len(plan.Moves))
	}
}

// ----------------------------------------------------------------------
// setActivePlan
// ----------------------------------------------------------------------

func TestSetActivePlan_NilPlan_NoActivePlanStored(t *testing.T) {
	pl := &SharedState{}

	pl.setActivePlan(nil, "id-ignored", nil)

	if got := pl.getActivePlan(); got != nil {
		t.Fatalf("setActivePlan(nil, ...) left active plan = %v, want nil", got)
	}
}

func TestSetActivePlan_ReplacesOldAndInitializesQuotas(t *testing.T) {
	pl := &SharedState{}

	// Old active plan with a Cancel that toggles a flag.
	oldCanceled := false
	oldAP := &ActivePlan{
		ID: "old",
		Cancel: func() {
			oldCanceled = true
		},
	}
	pl.ActivePlan.Store(oldAP)

	plan := &Plan{
		PlacementByName: map[string]string{
			mergeNsName("ns", "p1"): "n1",
		},
		WorkloadQuotas: WorkloadQuotas{
			"wk1": {
				"n1": 2,
				"n2": 0,
			},
		},
	}

	pl.setActivePlan(plan, "new-plan", nil)

	if !oldCanceled {
		t.Fatalf("old Cancel() was not called when replacing plan")
	}

	ap := pl.getActivePlan()
	if ap == nil {
		t.Fatalf("getActivePlan() = nil, want non-nil")
	}
	if ap.ID != "new-plan" {
		t.Fatalf("active plan ID = %q, want %q", ap.ID, "new-plan")
	}
	if ap.Ctx == nil || ap.Cancel == nil {
		t.Fatalf("active plan context or cancel is nil: ctx=%v cancel=%v", ap.Ctx, ap.Cancel)
	}

	// PlacementByName must be the same map (passed-through).
	if got := ap.PlacementByName[mergeNsName("ns", "p1")]; got != "n1" {
		t.Fatalf("PlacementByName[ns/p1] = %q, want %q", got, "n1")
	}

	// WorkloadPerNodeCnts should mirror WorkloadQuotas with atomics.
	perNode, ok := ap.WorkloadQuotas["wk1"]
	if !ok || perNode == nil {
		t.Fatalf("WorkloadPerNodeCnts[\"wk1\"] missing")
	}
	if ctr, ok := perNode["n1"]; !ok || ctr == nil || ctr.Load() != 2 {
		t.Fatalf("wk1 n1 counter = %v (val=%d), want non-nil and 2",
			perNode["n1"], func() int32 {
				if perNode["n1"] != nil {
					return perNode["n1"].Load()
				}
				return -1
			}())
	}
	if ctr, ok := perNode["n2"]; !ok || ctr == nil || ctr.Load() != 0 {
		t.Fatalf("wk1 n2 counter = %v (val=%d), want non-nil and 0",
			perNode["n2"], func() int32 {
				if perNode["n2"] != nil {
					return perNode["n2"].Load()
				}
				return -1
			}())
	}
}

// ----------------------------------------------------------------------
// buildWorkloadQuotasAtomics
// ----------------------------------------------------------------------

func TestBuildWorkloadQuotasAtomics_NilInput(t *testing.T) {
	got := buildWorkloadQuotas(nil)
	if got != nil {
		t.Fatalf("buildWorkloadQuotasAtomics(nil) = %#v, want nil", got)
	}
}

func TestBuildWorkloadQuotasAtomics_PositiveAndNonPositiveCounts(t *testing.T) {
	wk := WorkloadQuotas{
		"wk1": {
			"n1": 3,  // positive
			"n2": 0,  // zero
			"n3": -1, // negative
		},
	}

	got := buildWorkloadQuotas(wk)
	if got == nil {
		t.Fatalf("buildWorkloadQuotasAtomics(...) = nil, want non-nil")
	}

	perNode, ok := got["wk1"]
	if !ok || perNode == nil {
		t.Fatalf("got[\"wk1\"] = %#v, want non-nil map", got["wk1"])
	}

	if ctr := perNode["n1"]; ctr == nil || ctr.Load() != 3 {
		t.Fatalf("wk1 n1 ctr = %v (val=%d), want non-nil & 3",
			ctr, func() int32 {
				if ctr != nil {
					return ctr.Load()
				}
				return -1
			}())
	}
	if ctr := perNode["n2"]; ctr == nil || ctr.Load() != 0 {
		t.Fatalf("wk1 n2 ctr = %v (val=%d), want non-nil & 0",
			ctr, func() int32 {
				if ctr != nil {
					return ctr.Load()
				}
				return -1
			}())
	}
	if ctr := perNode["n3"]; ctr == nil || ctr.Load() != 0 {
		t.Fatalf("wk1 n3 ctr = %v (val=%d), want non-nil & 0",
			ctr, func() int32 {
				if ctr != nil {
					return ctr.Load()
				}
				return -1
			}())
	}
}

// ----------------------------------------------------------------------
// isPodAllowedByPlan
// ----------------------------------------------------------------------

func TestIsPodAllowedByPlan_NoActivePlan(t *testing.T) {
	pl := &SharedState{}
	p := newPod("ns", "p", "u", "", "", "", 1)

	if got := pl.isPodAllowedByPlan(p); got {
		t.Fatalf("isPodAllowedByPlan() with no active plan = %v, want false", got)
	}
}

func TestIsPodAllowedByPlan_PinnedStandalone(t *testing.T) {
	pl := &SharedState{}
	ap := &ActivePlan{
		ID: "plan",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p-standalone"): "n1",
		},
	}
	pl.ActivePlan.Store(ap)

	p := newPod("ns", "p-standalone", "u1", "", "", "", 1)

	if got := pl.isPodAllowedByPlan(p); !got {
		t.Fatalf("isPodAllowedByPlan() for pinned standalone pod = %v, want true", got)
	}
}

func TestIsPodAllowedByPlan_WorkloadAnyNodeAllowed(t *testing.T) {
	pl := &SharedState{}
	var prio int32 = 5

	p := newPod("ns", "p", "u1", "", "ReplicaSet", "rs-1", prio)

	wk, owned := getTopWorkload(p)
	if !owned {
		t.Fatalf("getTopWorkload() returned owned=false for owned pod")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {
			"n1": 1, // some quota
		},
	}

	ap := &ActivePlan{
		ID:             "plan",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}
	pl.ActivePlan.Store(ap)

	if got := pl.isPodAllowedByPlan(p); !got {
		t.Fatalf("isPodAllowedByPlan() = false, want true when any node has quota")
	}
}

func TestIsPodAllowedByPlan_WorkloadSpecificNodeRequired(t *testing.T) {
	pl := &SharedState{}
	var prio int32 = 5

	// Pod already has a node selected.
	p := newPod("ns", "p", "u1", "n2", "ReplicaSet", "rs-1", prio)

	wk, owned := getTopWorkload(p)
	if !owned {
		t.Fatalf("getTopWorkload() returned owned=false for owned pod")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {
			"n2": 1,
			"n1": 0,
		},
	}
	ap := &ActivePlan{
		ID:             "plan",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}
	pl.ActivePlan.Store(ap)

	if got := pl.isPodAllowedByPlan(p); !got {
		t.Fatalf("isPodAllowedByPlan() = false, want true when selected node has quota")
	}

	// Now exhaust quota on n2.
	ap.WorkloadQuotas[wkStr]["n2"].Store(0)
	if got := pl.isPodAllowedByPlan(p); got {
		t.Fatalf("isPodAllowedByPlan() = true, want false when selected node quota is 0")
	}
}

func TestIsPodAllowedByPlan_WorkloadQuotasExhausted(t *testing.T) {
	pl := &SharedState{}
	var prio int32 = 5

	p := newPod("ns", "p", "u1", "", "ReplicaSet", "rs-1", prio)

	wk, owned := getTopWorkload(p)
	if !owned {
		t.Fatalf("getTopWorkload() returned owned=false for owned pod")
	}
	wkStr := wk.String()

	// All quota counters are zero.
	wq := WorkloadQuotas{
		wkStr: {
			"n1": 0,
			"n2": 0,
		},
	}
	ap := &ActivePlan{
		ID:             "plan",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}
	pl.ActivePlan.Store(ap)

	if got := pl.isPodAllowedByPlan(p); got {
		t.Fatalf("isPodAllowedByPlan() = true, want false when all quotas are exhausted")
	}
}

// ----------------------------------------------------------------------
// filterNodes
// ----------------------------------------------------------------------

func TestFilterNodes_NoActivePlan(t *testing.T) {
	pl := &SharedState{}
	p := newPod("ns", "p", "u", "", "", "", 1)

	nodes, reason, ok := pl.filterNodes(p)
	if nodes != nil {
		t.Fatalf("filterNodes() nodes = %v, want nil", nodes)
	}
	if !ok {
		t.Fatalf("filterNodes() ok = %v, want true (no active plan, should not block)", ok)
	}
	if reason != InfoNoActivePlan {
		t.Fatalf("filterNodes() reason = %q, want %q", reason, InfoNoActivePlan)
	}
}

func TestFilterNodes_PinnedStandaloneToSpecificNode(t *testing.T) {
	pl := &SharedState{}
	ap := &ActivePlan{
		ID: "plan",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p1"): "n1",
		},
	}
	pl.ActivePlan.Store(ap)

	p := newPod("ns", "p1", "u1", "", "", "", 1)

	nodes, reason, ok := pl.filterNodes(p)
	if !ok {
		t.Fatalf("filterNodes() ok = false, want true for pinned pod")
	}
	if nodes == nil || nodes.Len() != 1 || !nodes.Has("n1") {
		t.Fatalf("filterNodes() nodes = %v, want {\"n1\"}", nodes)
	}
	if reason == "" {
		t.Fatalf("filterNodes() reason is empty, want descriptive string")
	}
}

func TestFilterNodes_PinnedStandaloneNoSpecificNode(t *testing.T) {
	pl := &SharedState{}
	ap := &ActivePlan{
		ID: "plan",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p-any"): "",
		},
	}
	pl.ActivePlan.Store(ap)

	p := newPod("ns", "p-any", "u1", "", "", "", 1)

	nodes, reason, ok := pl.filterNodes(p)
	if !ok {
		t.Fatalf("filterNodes() ok = false, want true for pinned pod with empty node")
	}
	if nodes != nil {
		t.Fatalf("filterNodes() nodes = %v, want nil (standalone; allowed by plan)", nodes)
	}
	if reason == "" {
		t.Fatalf("filterNodes() reason is empty, want descriptive string")
	}
}

func TestFilterNodes_PodNotInPlanBlocked(t *testing.T) {
	pl := &SharedState{}
	ap := &ActivePlan{
		ID:              "plan",
		PlacementByName: map[string]string{},
		// No WorkloadPerNodeCnts for non-owned pods.
	}
	pl.ActivePlan.Store(ap)

	p := newPod("ns", "p-unknown", "u1", "", "", "", 1)

	nodes, reason, ok := pl.filterNodes(p)
	if ok {
		t.Fatalf("filterNodes() ok = true, want false for pod not in plan")
	}
	if nodes != nil {
		t.Fatalf("filterNodes() nodes = %v, want nil", nodes)
	}
	if reason == "" {
		t.Fatalf("filterNodes() reason is empty, want descriptive string")
	}
}

func TestFilterNodes_WorkloadNodesAllowed(t *testing.T) {
	pl := &SharedState{}
	var prio int32 = 5

	p := newPod("ns", "p", "u1", "", "ReplicaSet", "rs-1", prio)

	wk, owned := getTopWorkload(p)
	if !owned {
		t.Fatalf("getTopWorkload() returned owned=false for owned pod")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {
			"n1": 1,
			"n2": 0,
		},
	}

	ap := &ActivePlan{
		ID:             "plan",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}
	pl.ActivePlan.Store(ap)

	nodes, reason, ok := pl.filterNodes(p)
	if !ok {
		t.Fatalf("filterNodes() ok = false, want true; reason=%q", reason)
	}
	if nodes == nil || !nodes.Has("n1") || nodes.Has("n2") {
		t.Fatalf("filterNodes() nodes = %v, want only {n1}", nodes)
	}
}

func TestFilterNodes_WorkloadQuotasExhausted(t *testing.T) {
	pl := &SharedState{}
	var prio int32 = 5

	p := newPod("ns", "p", "u1", "", "ReplicaSet", "rs-1", prio)

	wk, owned := getTopWorkload(p)
	if !owned {
		t.Fatalf("getTopWorkload() returned owned=false for owned pod")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {
			"n1": 0,
		},
	}

	ap := &ActivePlan{
		ID:             "plan",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}
	pl.ActivePlan.Store(ap)

	nodes, reason, ok := pl.filterNodes(p)
	if ok {
		t.Fatalf("filterNodes() ok = true, want false when all quotas exhausted; reason=%q", reason)
	}
	if nodes != nil && nodes.Len() != 0 {
		t.Fatalf("filterNodes() nodes = %v, want nil or empty set", nodes)
	}
}

// ----------------------------------------------------------------------
// countNewAndTotalPods
// ----------------------------------------------------------------------

func TestCountNewAndTotalPods_NilOutput(t *testing.T) {
	pendingSched, pre, post := computePlanPodCounts(nil, nil)
	if pendingSched != 0 || pre != 0 || post != 0 {
		t.Fatalf("countNewAndTotalPods(nil, nil) = (%d,%d,%d), want (0,0,0)",
			pendingSched, pre, post)
	}
}

func TestCountNewAndTotalPods_BasicScenario(t *testing.T) {
	now := metav1.Now()

	// running pods
	run1 := newPod("ns", "run1", "u-run1", "n1", "", "", 1)
	run2 := newPod("ns", "run2", "u-run2", "n2", "", "", 1)
	// pending pod (to be scheduled)
	pend1 := newPod("ns", "pend1", "u-pend1", "", "", "", 1)
	// deleted pod (should be ignored)
	del := newPod("ns", "del", "u-del", "n1", "", "", 1)
	del.DeletionTimestamp = &now

	pods := []*v1.Pod{run1, run2, pend1, del}

	out := &SolverOutput{
		Placements: []SolverPod{
			{UID: pend1.UID, Node: "n1"}, // schedule pending
			{UID: "u-unknown", Node: "n1"},
		},
		Evictions: []SolverPod{
			{UID: run1.UID}, // evict one running
			{UID: "u-nonexistent"},
		},
	}

	pendingScheduled, totalPre, totalPost := computePlanPodCounts(out, pods)

	// Pre-plan: two running (run1, run2).
	if totalPre != 2 {
		t.Fatalf("totalPrePlan = %d, want 2", totalPre)
	}
	// We schedule one pending.
	if pendingScheduled != 1 {
		t.Fatalf("pendingScheduled = %d, want 1", pendingScheduled)
	}
	// Post = running (2) - evicted(1) + scheduled(1) = 2
	if totalPost != 2 {
		t.Fatalf("totalPostPlan = %d, want 2", totalPost)
	}
}

func TestCountNewAndTotalPods_TotalPostNonNegative(t *testing.T) {
	// Only pending pods → totalPrePlan = 0
	pend := newPod("ns", "pend", "u-pend", "", "", "", 1)
	pods := []*v1.Pod{pend}

	// Eviction references a UID not present or with no node
	out := &SolverOutput{
		Evictions: []SolverPod{
			{UID: "u-missing"},
		},
	}

	pendingScheduled, totalPre, totalPost := computePlanPodCounts(out, pods)
	if totalPre != 0 {
		t.Fatalf("totalPrePlan = %d, want 0", totalPre)
	}
	if pendingScheduled != 0 {
		t.Fatalf("pendingScheduled = %d, want 0", pendingScheduled)
	}
	if totalPost != 0 {
		t.Fatalf("totalPostPlan = %d, want 0 (clamped, not negative)", totalPost)
	}
}

// ----------------------------------------------------------------------
// evictTargets
// ----------------------------------------------------------------------

func TestEvictTargets_UsesHook(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p1 := newPod("ns", "p1", "u1", "", "", "", 1)
	p2 := newPod("ns", "p2", "u2", "", "", "", 1)
	targets := []*v1.Pod{p1, p2}

	var (
		called     bool
		gotPl      *SharedState
		gotCtx     context.Context
		gotTargets []*v1.Pod
	)

	orig := evictTargetsHook
	defer func() { evictTargetsHook = orig }()

	evictTargetsHook = func(hpl *SharedState, hctx context.Context, htargets []*v1.Pod) error {
		called = true
		gotPl = hpl
		gotCtx = hctx
		gotTargets = htargets
		return nil
	}

	if err := pl.evictTargets(ctx, targets); err != nil {
		t.Fatalf("evictTargets() error = %v, want nil", err)
	}
	if !called {
		t.Fatalf("evictTargetsHook not called")
	}
	if gotPl != pl {
		t.Fatalf("hook pl = %p, want %p", gotPl, pl)
	}
	if gotCtx != ctx {
		t.Fatalf("hook ctx = %v, want %v", gotCtx, ctx)
	}
	if !reflect.DeepEqual(gotTargets, targets) {
		t.Fatalf("hook targets = %#v, want %#v", gotTargets, targets)
	}
}

func TestEvictTargets_NormalAndNotFoundAreIgnored(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p1 := newPod("ns", "p1", "u1", "", "", "", 1)
	p2 := newPod("ns", "p2", "u2", "", "", "", 1)

	var seen []string

	withEvictHook(func(_ *SharedState, _ context.Context, pod *v1.Pod, _ *policyv1.Eviction) error {
		seen = append(seen, mergeNsName(pod.Namespace, pod.Name))
		// Alternate between nil and NotFound to exercise the IsNotFound branch.
		if pod.Name == "p1" {
			return nil
		}
		return apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, pod.Name)
	}, func() {
		if err := pl.evictTargets(ctx, []*v1.Pod{p1, p2}); err != nil {
			t.Fatalf("evictTargets() error = %v, want nil", err)
		}
	})

	if len(seen) != 2 {
		t.Fatalf("evictPodFor called %d times, want 2", len(seen))
	}
}

func TestEvictTargets_PropagatesNonNotFoundError(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p := newPod("ns", "p", "u", "", "", "", 1)
	wantErr := fmt.Errorf("boom-evict")

	withEvictHook(func(_ *SharedState, _ context.Context, _ *v1.Pod, _ *policyv1.Eviction) error {
		return wantErr
	}, func() {
		err := pl.evictTargets(ctx, []*v1.Pod{p})
		if err == nil {
			t.Fatalf("evictTargets() error = nil, want non-nil")
		}
		if !strings.Contains(err.Error(), "boom-evict") {
			t.Fatalf("evictTargets() error = %v, want to contain %q", err, "boom-evict")
		}
	})
}

// ----------------------------------------------------------------------
// waitPodsGone
// ---------------------------------------------------------------------

func TestWaitPodsGone_UsesHookWhenNonEmpty(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p := newPod("ns", "p", "u1", "n1", "", "", 1)

	var (
		called bool
		gotPl  *SharedState
		gotCtx context.Context
		got    []*v1.Pod
	)

	orig := waitPodsGoneHook
	defer func() { waitPodsGoneHook = orig }()

	waitPodsGoneHook = func(hpl *SharedState, hctx context.Context, pods []*v1.Pod) error {
		called = true
		gotPl = hpl
		gotCtx = hctx
		got = pods
		return nil
	}

	if err := pl.waitPodsGone(ctx, []*v1.Pod{p}); err != nil {
		t.Fatalf("waitPodsGone() error = %v, want nil", err)
	}
	if !called {
		t.Fatalf("waitPodsGoneHook not called")
	}
	if gotPl != pl {
		t.Fatalf("hook pl = %p, want %p", gotPl, pl)
	}
	if gotCtx != ctx {
		t.Fatalf("hook ctx = %v, want %v", gotCtx, ctx)
	}
	if len(got) != 1 || got[0] != p {
		t.Fatalf("hook pods = %#v, want [%#v]", got, p)
	}
}

func TestWaitPodsGone_EmptySliceFastPath(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	// No hook: this must return immediately without error.
	if err := pl.waitPodsGone(ctx, nil); err != nil {
		t.Fatalf("waitPodsGone(nil) error = %v, want nil", err)
	}
}

func TestWaitPodsGone_PollUntilNotFound(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p := newPod("ns", "p", "u1", "", "", "", 1)

	// Fake lister that always returns IsNotFound for this pod.
	store := map[string]map[string]*v1.Pod{} // empty store → NotFound via fakePodNamespaceLister
	plister := &fakePodLister{store: store}

	withPodLister(plister, func() {
		if err := pl.waitPodsGone(ctx, []*v1.Pod{p}); err != nil {
			t.Fatalf("waitPodsGone() error = %v, want nil", err)
		}
	})
}

func TestWaitPodsGone_IgnoresNilPodsInInput(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	// Input slice includes a nil pod; this should not panic and should
	// effectively behave like an empty slice.
	if err := pl.waitPodsGone(ctx, []*v1.Pod{nil}); err != nil {
		t.Fatalf("waitPodsGone() with nil pod error = %v, want nil", err)
	}
}

func TestWaitPodsGone_TreatsUidChangeOrTerminatingAsGone(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	// Original pod identity (UID + ns + name)
	orig := newPod("ns", "p", "u1", "", "", "", 1)

	// In the store we simulate the "same" pod name but:
	//  - different UID (u2)
	//  - or terminating
	// Either should make the entry be considered "gone".
	changedUID := newPod("ns", "p", "u2", "", "", "", 1)
	now := metav1.Now()
	terminating := newPod("ns", "p-term", "u3", "", "", "", 1)
	terminating.DeletionTimestamp = &now

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p":      changedUID,
			"p-term": terminating,
		},
	}
	plister := &fakePodLister{store: store}

	withPodLister(plister, func() {
		// First: UID has changed → treated as gone.
		if err := pl.waitPodsGone(ctx, []*v1.Pod{orig}); err != nil {
			t.Fatalf("waitPodsGone() (UID changed) error = %v, want nil", err)
		}

		// Second: pod terminating → treated as gone as well.
		if err := pl.waitPodsGone(ctx, []*v1.Pod{terminating}); err != nil {
			t.Fatalf("waitPodsGone() (terminating) error = %v, want nil", err)
		}
	})
}

// ----------------------------------------------------------------------
// activatePods
// ----------------------------------------------------------------------

func TestActivatePods_NoPodSetDoesNothing(t *testing.T) {
	pl := &SharedState{}

	var called bool
	orig := activatePods
	defer func() { activatePods = orig }()

	activatePods = func(_ *SharedState, _ map[string]*v1.Pod) {
		called = true
	}

	// nil podSet → no activation
	tried := pl.activatePods(nil, true, -1)
	if len(tried) != 0 {
		t.Fatalf("activatePods(nil) tried = %v, want empty", tried)
	}
	if called {
		t.Fatalf("activatePods() global should not be called when podSet is nil")
	}
}

// ----------------------------------------------------------------------
// activatePlannedPods
// ----------------------------------------------------------------------

func TestActivatePlannedPods_UsesHookWithMatchingPending(t *testing.T) {
	pl := &SharedState{}

	uidAllowed := types.UID("u-allowed")
	uidIgnored := types.UID("u-ignored")

	plan := &Plan{
		NewPlacements: []SolverPod{
			{UID: uidAllowed, OldNode: "", Node: "n1"},      // should be activated
			{UID: uidIgnored, OldNode: "n-old", Node: "n2"}, // move, not pending→node
		},
	}

	pending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "p-allowed",
			UID:       uidAllowed,
		},
		Spec: v1.PodSpec{
			NodeName: "",
		},
	}
	running := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "p-running",
			UID:       uidIgnored,
		},
		Spec: v1.PodSpec{
			NodeName: "n-old",
		},
	}

	pods := []*v1.Pod{pending, running}

	var (
		called bool
		got    map[string]*v1.Pod
	)

	orig := activatePlannedPodsHook
	defer func() { activatePlannedPodsHook = orig }()

	activatePlannedPodsHook = func(hpl *SharedState, toActivate map[string]*v1.Pod) {
		if hpl != pl {
			t.Fatalf("hook pl = %p, want %p", hpl, pl)
		}
		called = true
		got = toActivate
	}

	pl.activatePlannedPods(plan, pods)

	if !called {
		t.Fatalf("activatePlannedPodsHook not called")
	}
	if len(got) != 1 {
		t.Fatalf("toActivate len = %d, want 1", len(got))
	}
	key := mergeNsName("ns", "p-allowed")
	if got[key] != pending {
		t.Fatalf("toActivate[%q] = %#v, want %#v", key, got[key], pending)
	}
}

func TestActivatePlannedPods_NoPlanOrNoNewPlacements(t *testing.T) {
	pl := &SharedState{}

	// No panic, no hook called → just exercise the guard clauses.
	pl.activatePlannedPods(nil, nil)
	pl.activatePlannedPods(&Plan{}, nil)
	pl.activatePlannedPods(&Plan{NewPlacements: nil}, []*v1.Pod{})
}

func TestActivatePlannedPods_UsesGlobalActivateWhenNoHook(t *testing.T) {
	pl := &SharedState{}

	uid := types.UID("u-allowed")
	plan := &Plan{
		NewPlacements: []SolverPod{
			{UID: uid, OldNode: "", Node: "n1"}, // From "" -> Node != "" => newly scheduled
		},
	}
	pending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "p-allowed",
			UID:       uid,
		},
		Spec: v1.PodSpec{
			NodeName: "", // pending
		},
	}

	var (
		called bool
		gotPl  *SharedState
		gotMap map[string]*v1.Pod
	)

	orig := activatePods
	defer func() { activatePods = orig }()

	activatePods = func(hpl *SharedState, toAct map[string]*v1.Pod) {
		called = true
		gotPl = hpl
		gotMap = toAct
	}

	pl.activatePlannedPods(plan, []*v1.Pod{pending})

	if !called {
		t.Fatalf("global activatePods was not called")
	}
	if gotPl != pl {
		t.Fatalf("activatePods pl = %p, want %p", gotPl, pl)
	}
	key := mergeNsName("ns", "p-allowed")
	if gotMap[key] != pending {
		t.Fatalf("toAct[%q] = %#v, want %#v", key, gotMap[key], pending)
	}
}

// ----------------------------------------------------------------------
// isPlanCompleted
// ----------------------------------------------------------------------

func TestIsPlanCompleted_UsesHook(t *testing.T) {
	pl := &SharedState{}
	ap := &ActivePlan{ID: PlanConfigMapNamePrefix + "1"}

	var (
		called bool
		gotPl  *SharedState
		gotAp  *ActivePlan
	)

	orig := isPlanCompletedHook
	defer func() { isPlanCompletedHook = orig }()

	isPlanCompletedHook = func(hpl *SharedState, hap *ActivePlan) (bool, error) {
		called = true
		gotPl = hpl
		gotAp = hap
		return true, nil
	}

	ok, err := pl.isPlanCompleted(ap)
	if err != nil {
		t.Fatalf("isPlanCompleted() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("isPlanCompleted() = false, want true from hook")
	}
	if !called {
		t.Fatalf("isPlanCompletedHook not called")
	}
	if gotPl != pl || gotAp != ap {
		t.Fatalf("hook args = (%p,%p), want (%p,%p)", gotPl, gotAp, pl, ap)
	}
}

func TestIsPlanCompleted_NilActivePlan(t *testing.T) {
	pl := &SharedState{}

	ok, err := pl.isPlanCompleted(nil)
	if err != nil {
		t.Fatalf("isPlanCompleted(nil) error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("isPlanCompleted(nil) = true, want false (no active plan doc)")
	}
}

func TestIsPlanCompleted_PinnedPodGoneAndNoQuotasMeansCompleted(t *testing.T) {
	pl := &SharedState{}

	ap := &ActivePlan{
		ID: PlanConfigMapNamePrefix + "1",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p-gone"): "n1",
		},
		WorkloadQuotas: WorkloadQuotasAtomics{}, // no workload quotas
	}

	// Empty store -> Get() returns NotFound for ns/p-gone.
	store := map[string]map[string]*v1.Pod{}
	plister := &fakePodLister{store: store}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("isPlanCompleted() = false, want true (pinned pod gone, no quotas)")
		}
	})
}

func TestIsPlanCompleted_PinnedPodOnWrongNodeNotCompleted(t *testing.T) {
	pl := &SharedState{}

	ap := &ActivePlan{
		ID: PlanConfigMapNamePrefix + "1",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p-mismatch"): "n-expected",
		},
	}

	p := newPod("ns", "p-mismatch", "u1", "n-other", "", "", 1)

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-mismatch": p,
		},
	}
	plister := &fakePodLister{store: store}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if ok {
			t.Fatalf("isPlanCompleted() = true, want false due to pinned pod on wrong node")
		}
	})
}

func TestIsPlanCompleted_PinnedPodTerminatingTreatedAsSatisfied(t *testing.T) {
	pl := &SharedState{}

	ap := &ActivePlan{
		ID: PlanConfigMapNamePrefix + "1",
		PlacementByName: map[string]string{
			mergeNsName("ns", "p-term"): "n1",
		},
		WorkloadQuotas: WorkloadQuotasAtomics{},
	}

	p := newPod("ns", "p-term", "u1", "n1", "", "", 1)
	now := metav1.Now()
	p.DeletionTimestamp = &now

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-term": p,
		},
	}
	plister := &fakePodLister{store: store}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("isPlanCompleted() = false, want true (terminating pod treated as satisfied)")
		}
	})
}

func TestIsPlanCompleted_WorkloadScaledDownIgnored(t *testing.T) {
	pl := &SharedState{}

	// No live pods at all.
	store := map[string]map[string]*v1.Pod{}
	plister := &fakePodLister{store: store}

	// Build a workload key from a dummy pod so we get the right string form.
	pDummy := newPod("ns", "p", "u-dummy", "", "ReplicaSet", "rs-1", 1)
	wk, owned := getTopWorkload(pDummy)
	if !owned {
		t.Fatalf("getTopWorkload(dummy) returned owned=false")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {"n1": 3}, // remaining quota > 0
	}

	ap := &ActivePlan{
		ID:             PlanConfigMapNamePrefix + "1",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("isPlanCompleted() = false, want true (workload scaled down, remaining quota ignored)")
		}
	})
}

func TestIsPlanCompleted_WorkloadPendingWithQuotaNotCompleted(t *testing.T) {
	pl := &SharedState{}

	// Running + pending pods for the same workload.
	var prio int32 = 5
	pRun := newPod("ns", "p-run", "u-run", "n1", "ReplicaSet", "rs-1", prio)
	pPending := newPod("ns", "p-pend", "u-pend", "", "ReplicaSet", "rs-1", prio)

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-run":  pRun,
			"p-pend": pPending,
		},
	}
	plister := &fakePodLister{store: store}

	wk, owned := getTopWorkload(pRun)
	if !owned {
		t.Fatalf("getTopWorkload(run) returned owned=false")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {"n1": 1}, // remaining quota
	}

	ap := &ActivePlan{
		ID:             PlanConfigMapNamePrefix + "1",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if ok {
			t.Fatalf("isPlanCompleted() = true, want false (pending pods + remaining quota)")
		}
	})
}

func TestIsPlanCompleted_WorkloadLiveNoPendingQuotaSatisfied(t *testing.T) {
	pl := &SharedState{}

	// Only running pods for the workload.
	var prio int32 = 5
	pRun := newPod("ns", "p-run", "u-run", "n1", "ReplicaSet", "rs-1", prio)

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-run": pRun,
		},
	}
	plister := &fakePodLister{store: store}

	wk, owned := getTopWorkload(pRun)
	if !owned {
		t.Fatalf("getTopWorkload(run) returned owned=false")
	}
	wkStr := wk.String()

	wq := WorkloadQuotas{
		wkStr: {"n1": 2}, // remaining > 0
	}

	ap := &ActivePlan{
		ID:             PlanConfigMapNamePrefix + "1",
		WorkloadQuotas: buildWorkloadQuotas(wq),
	}

	withPodLister(plister, func() {
		ok, err := pl.isPlanCompleted(ap)
		if err != nil {
			t.Fatalf("isPlanCompleted() error = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("isPlanCompleted() = false, want true (live pods, no pending, remaining quota treated as satisfied)")
		}
	})
}

// ----------------------------------------------------------------------
// onPlanCompleted
// ----------------------------------------------------------------------

func TestOnPlanCompleted_HookCalledAfterStateCleared(t *testing.T) {
	pl := &SharedState{}
	pl.ActivePlanInProgress.Store(true)

	cancelled := false
	ap := &ActivePlan{
		ID: PlanConfigMapNamePrefix + "1",
		Cancel: func() {
			cancelled = true
		},
	}
	pl.ActivePlan.Store(ap)

	var (
		called           bool
		hookStatus       PlanStatus
		hookAp           *ActivePlan
		activeInsideHook bool
	)

	orig := onPlanCompletedHook
	defer func() { onPlanCompletedHook = orig }()

	onPlanCompletedHook = func(hpl *SharedState, status PlanStatus, hap *ActivePlan) {
		if hpl != pl {
			t.Fatalf("hook pl = %p, want %p", hpl, pl)
		}
		called = true
		hookStatus = status
		hookAp = hap
		activeInsideHook = hpl.ActivePlanInProgress.Load()
	}

	ok := pl.onPlanCompleted(PlanStatusCompleted)
	if !ok {
		t.Fatalf("onPlanCompleted() = false, want true")
	}
	if !called {
		t.Fatalf("onPlanCompletedHook not called")
	}
	if hookStatus != PlanStatusCompleted {
		t.Fatalf("hook status = %v, want %v", hookStatus, PlanStatusCompleted)
	}
	if hookAp != ap {
		t.Fatalf("hook ap = %p, want %p", hookAp, ap)
	}
	if activeInsideHook {
		t.Fatalf("Active flag true inside hook, want false after leaveActive")
	}
	if cancelled != true {
		t.Fatalf("ap.Cancel was not called")
	}
	if pl.getActivePlan() != nil {
		t.Fatalf("ActivePlan not cleared by onPlanCompleted")
	}
}

func TestOnPlanCompleted_NoActivePlanOrAlreadyCleared(t *testing.T) {
	pl := &SharedState{}

	// No ActivePlan stored -> tryClearActivePlan(nil) == false.
	ok := pl.onPlanCompleted(PlanStatusCompleted)
	if ok {
		t.Fatalf("onPlanCompleted() = true, want false when there is no active plan")
	}
}

func TestOnPlanCompleted_DefaultPathTearsDownAndMarksStatus(t *testing.T) {
	pl := &SharedState{}
	pl.ActivePlanInProgress.Store(true)

	cancelled := false
	ap := &ActivePlan{
		ID: PlanConfigMapNamePrefix + "cm-123",
		Cancel: func() {
			cancelled = true
		},
	}
	pl.ActivePlan.Store(ap)

	// Ensure there is no hook so the default path is taken.
	origOnHook := onPlanCompletedHook
	defer func() { onPlanCompletedHook = origOnHook }()
	onPlanCompletedHook = nil

	var (
		statusCalled bool
		gotPl        *SharedState
		gotCtx       context.Context
		gotCM        string
		gotStatus    PlanStatus
	)
	origMark := markPlanStatusToConfigMapHook
	defer func() { markPlanStatusToConfigMapHook = origMark }()

	markPlanStatusToConfigMapHook = func(hpl *SharedState, hctx context.Context, planCM string, status PlanStatus) bool {
		statusCalled = true
		gotPl = hpl
		gotCtx = hctx
		gotCM = planCM
		gotStatus = status
		return true // skip real mutateRaw
	}

	ok := pl.onPlanCompleted(PlanStatusCompleted)
	if !ok {
		t.Fatalf("onPlanCompleted() = false, want true")
	}
	if pl.getActivePlan() != nil {
		t.Fatalf("active plan not cleared")
	}
	if pl.ActivePlanInProgress.Load() {
		t.Fatalf("ActivePlanInProgress still true after onPlanCompleted")
	}
	if !cancelled {
		t.Fatalf("ActivePlan.Cancel was not called")
	}
	if !statusCalled {
		t.Fatalf("markPlanStatusToConfigMapHook not called")
	}
	if gotPl != pl {
		t.Fatalf("markPlanStatusToConfigMapHook pl = %p, want %p", gotPl, pl)
	}
	if gotCtx == nil {
		t.Fatalf("markPlanStatusToConfigMapHook ctx is nil")
	}
	if gotCM != ap.ID {
		t.Fatalf("markPlanStatusToConfigMapHook planCM = %q, want %q", gotCM, ap.ID)
	}
	if gotStatus != PlanStatusCompleted {
		t.Fatalf("markPlanStatusToConfigMapHook status = %v, want %v", gotStatus, PlanStatusCompleted)
	}
}

// ----------------------------------------------------------------------
// exportPlanToConfigMap
// ----------------------------------------------------------------------

func TestExportPlanToConfigMap_UsesHook(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()
	sp := &StoredPlan{}
	name := PlanConfigMapNamePrefix + "cm"

	var (
		called  bool
		gotPl   *SharedState
		gotCtx  context.Context
		gotName string
		gotSp   *StoredPlan
	)

	orig := exportPlanToConfigMapHook
	defer func() { exportPlanToConfigMapHook = orig }()

	exportPlanToConfigMapHook = func(hpl *SharedState, hctx context.Context, hname string, hsp *StoredPlan) error {
		called = true
		gotPl = hpl
		gotCtx = hctx
		gotName = hname
		gotSp = hsp
		return nil
	}

	if err := pl.exportPlanToConfigMap(ctx, name, sp); err != nil {
		t.Fatalf("exportPlanToConfigMap() error = %v, want nil", err)
	}
	if !called {
		t.Fatalf("exportPlanToConfigMapHook not called")
	}
	if gotPl != pl || gotCtx != ctx || gotName != name || gotSp != sp {
		t.Fatalf("hook args mismatch")
	}
}

// ----------------------------------------------------------------------
// markPlanStatusToConfigMap
// ----------------------------------------------------------------------

func TestMarkPlanStatusToConfigMap_UsesHook(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	var (
		called    bool
		gotPl     *SharedState
		gotCtx    context.Context
		gotCM     string
		gotStatus PlanStatus
	)

	orig := markPlanStatusToConfigMapHook
	defer func() { markPlanStatusToConfigMapHook = orig }()

	markPlanStatusToConfigMapHook = func(hpl *SharedState, hctx context.Context, planCM string, status PlanStatus) bool {
		called = true
		gotPl = hpl
		gotCtx = hctx
		gotCM = planCM
		gotStatus = status
		return true // tell implementation to skip real mutateRaw
	}

	pl.setPlanStatusInConfigMap(ctx, "cm-name", PlanStatusFailed)

	if !called {
		t.Fatalf("markPlanStatusHook not called")
	}
	if gotPl != pl || gotCtx != ctx || gotCM != "cm-name" || gotStatus != PlanStatusFailed {
		t.Fatalf("hook args mismatch")
	}
}

func newSharedStateWithConfigMapInformer(t *testing.T, objects ...runtime.Object) (*SharedState, func()) {
	t.Helper()

	client := fake.NewSimpleClientset(objects...)
	factory := informers.NewSharedInformerFactory(client, 0)
	cmInformer := factory.Core().V1().ConfigMaps().Informer()

	stopCh := make(chan struct{})
	factory.Start(stopCh)
	if ok := cache.WaitForCacheSync(stopCh, cmInformer.HasSynced); !ok {
		close(stopCh)
		t.Fatalf("ConfigMap informer failed to sync")
	}

	h := &fakeHandle{client: client, factory: factory}
	pl := &SharedState{Client: client, Handle: h}

	cleanup := func() { close(stopCh) }
	return pl, cleanup
}

func TestExportPlanToConfigMap_Default_CreatesConfigMapWithJson(t *testing.T) {
	orig := exportPlanToConfigMapHook
	defer func() { exportPlanToConfigMapHook = orig }()
	exportPlanToConfigMapHook = nil

	pl, cleanup := newSharedStateWithConfigMapInformer(t)
	defer cleanup()

	ctx := context.Background()
	name := "plan-cm-default"
	sp := &StoredPlan{
		PluginVersion:        "test",
		OptimizationStrategy: "unit",
		GeneratedAt:          time.Unix(123, 0).UTC(),
		PlanStatus:           PlanStatusActive,
		SolverResult:         SolverResult{Status: "ok"},
		Plan:                 &Plan{},
	}

	if err := pl.exportPlanToConfigMap(ctx, name, sp); err != nil {
		t.Fatalf("exportPlanToConfigMap() error = %v, want nil", err)
	}

	cm, err := pl.Client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap error = %v", err)
	}
	if cm.Labels == nil || cm.Labels[PlanConfigMapLabelKey] != "true" {
		t.Fatalf("ConfigMap label %q=%q, want %q", PlanConfigMapLabelKey, cm.Labels[PlanConfigMapLabelKey], "true")
	}
	dataKey := PlanConfigMapLabelKey + ".json"
	raw, ok := cm.Data[dataKey]
	if !ok {
		t.Fatalf("ConfigMap missing data key %q", dataKey)
	}

	var got StoredPlan
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal stored plan error = %v", err)
	}
	if got.PlanStatus != PlanStatusActive {
		t.Fatalf("stored plan status = %v, want %v", got.PlanStatus, PlanStatusActive)
	}
	if got.PluginVersion != "test" {
		t.Fatalf("stored plan PluginVersion = %q, want %q", got.PluginVersion, "test")
	}
}

func TestExportPlanToConfigMap_Default_PrunesOldPlans(t *testing.T) {
	orig := exportPlanToConfigMapHook
	defer func() { exportPlanToConfigMapHook = orig }()
	exportPlanToConfigMapHook = nil

	// Seed PlansToRetain+1 labeled ConfigMaps to ensure pruning runs.
	objs := make([]runtime.Object, 0, PlansToRetain+1)
	base := time.Now().UTC()
	for i := 0; i < PlansToRetain+1; i++ {
		name := "plan-retain-" + string(rune('a'+(i%26)))
		if i >= 26 {
			name = "plan-retain-" + string(rune('a'+((i/26)%26))) + string(rune('a'+(i%26)))
		}
		cm := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         SystemNamespace,
				Name:              name,
				Labels:            map[string]string{PlanConfigMapLabelKey: "true"},
				CreationTimestamp: metav1.NewTime(base.Add(-time.Duration(i) * time.Minute)),
			},
			Data: map[string]string{PlanConfigMapLabelKey + ".json": "{}"},
		}
		objs = append(objs, cm)
	}

	pl, cleanup := newSharedStateWithConfigMapInformer(t, objs...)
	defer cleanup()
	ctx := context.Background()

	// Update the newest plan so ensureJson doesn't change object count.
	newestName := "plan-retain-a"
	sp := &StoredPlan{PluginVersion: "test", GeneratedAt: time.Unix(1, 0).UTC(), PlanStatus: PlanStatusActive, Plan: &Plan{}}

	if err := pl.exportPlanToConfigMap(ctx, newestName, sp); err != nil {
		t.Fatalf("exportPlanToConfigMap() error = %v, want nil", err)
	}

	list, err := pl.Client.CoreV1().ConfigMaps(SystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List ConfigMaps error = %v", err)
	}

	// Count labeled plan CMs.
	count := 0
	for _, cm := range list.Items {
		if cm.Labels != nil && cm.Labels[PlanConfigMapLabelKey] == "true" {
			count++
		}
	}
	if count != PlansToRetain {
		t.Fatalf("labeled plan ConfigMaps = %d, want %d", count, PlansToRetain)
	}
}

func TestSetPlanStatusInConfigMap_Default_SetsStatusAndCompletedAt(t *testing.T) {
	orig := markPlanStatusToConfigMapHook
	defer func() { markPlanStatusToConfigMapHook = orig }()
	markPlanStatusToConfigMapHook = nil

	name := "plan-status-cm"
	sp := &StoredPlan{PluginVersion: "test", GeneratedAt: time.Unix(1, 0).UTC(), PlanStatus: PlanStatusActive, Plan: &Plan{}}
	b, _ := json.MarshalIndent(sp, "", "  ")
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: SystemNamespace,
			Name:      name,
			Labels:    map[string]string{PlanConfigMapLabelKey: "true"},
		},
		Data: map[string]string{PlanConfigMapLabelKey + ".json": string(b)},
	}

	pl, cleanup := newSharedStateWithConfigMapInformer(t, cm)
	defer cleanup()
	ctx := context.Background()

	pl.setPlanStatusInConfigMap(ctx, name, PlanStatusCompleted)

	gotCM, err := pl.Client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap error = %v", err)
	}
	raw := gotCM.Data[PlanConfigMapLabelKey+".json"]

	var got StoredPlan
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal stored plan error = %v", err)
	}
	if got.PlanStatus != PlanStatusCompleted {
		t.Fatalf("status = %v, want %v", got.PlanStatus, PlanStatusCompleted)
	}
	if got.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt is zero, want non-zero")
	}
}

func TestSetPlanStatusInConfigMap_Default_FinalIsSticky(t *testing.T) {
	orig := markPlanStatusToConfigMapHook
	defer func() { markPlanStatusToConfigMapHook = orig }()
	markPlanStatusToConfigMapHook = nil

	name := "plan-status-sticky"
	completedAt := time.Unix(10, 0).UTC()
	sp := &StoredPlan{
		PluginVersion: "test",
		GeneratedAt:   time.Unix(1, 0).UTC(),
		CompletedAt:   completedAt,
		PlanStatus:    PlanStatusFailed,
		Plan:          &Plan{},
	}
	b, _ := json.MarshalIndent(sp, "", "  ")
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: SystemNamespace,
			Name:      name,
			Labels:    map[string]string{PlanConfigMapLabelKey: "true"},
		},
		Data: map[string]string{PlanConfigMapLabelKey + ".json": string(b)},
	}

	pl, cleanup := newSharedStateWithConfigMapInformer(t, cm)
	defer cleanup()
	ctx := context.Background()

	pl.setPlanStatusInConfigMap(ctx, name, PlanStatusCompleted)

	gotCM, err := pl.Client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap error = %v", err)
	}
	raw := gotCM.Data[PlanConfigMapLabelKey+".json"]

	var got StoredPlan
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal stored plan error = %v", err)
	}
	if got.PlanStatus != PlanStatusFailed {
		t.Fatalf("status = %v, want %v (sticky)", got.PlanStatus, PlanStatusFailed)
	}
	if !got.CompletedAt.Equal(completedAt) {
		t.Fatalf("CompletedAt = %v, want %v (unchanged)", got.CompletedAt, completedAt)
	}
}

func TestSetPlanStatusInConfigMap_Default_InvalidJsonIsBestEffortNoop(t *testing.T) {
	orig := markPlanStatusToConfigMapHook
	defer func() { markPlanStatusToConfigMapHook = orig }()
	markPlanStatusToConfigMapHook = nil

	name := "plan-status-invalid-json"
	bad := "{"
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: SystemNamespace,
			Name:      name,
			Labels:    map[string]string{PlanConfigMapLabelKey: "true"},
		},
		Data: map[string]string{PlanConfigMapLabelKey + ".json": bad},
	}

	pl, cleanup := newSharedStateWithConfigMapInformer(t, cm)
	defer cleanup()
	ctx := context.Background()

	pl.setPlanStatusInConfigMap(ctx, name, PlanStatusCompleted)

	gotCM, err := pl.Client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap error = %v", err)
	}
	if gotCM.Data[PlanConfigMapLabelKey+".json"] != bad {
		t.Fatalf("expected invalid json to remain unchanged")
	}
}
