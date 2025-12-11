// plan_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	want := PlannerPod{
		UID:       p.UID,
		Namespace: p.Namespace,
		Name:      p.Name,
	}
	if !reflect.DeepEqual(pp, want) {
		t.Fatalf("toPlanPod() = %+v, want %+v", pp, want)
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
	p1 := newPod("ns", "p1", "u1", "n1", 1)
	p2 := newPod("ns", "p2", "u2", "n1", 1)
	p3 := newPod("ns", "p3", "u3", "", 1)

	out := &PlannerOutput{
		Evictions: []PlannerPod{
			{UID: p1.UID},
		},
		Placements: []PlannerPod{
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

	pre := newPod("ns", "pre", "pre-uid", "", 1)

	out := &PlannerOutput{
		Placements: []PlannerPod{
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
	p := newPod("ns", "p", "u", "", 1)

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

	p := newPod("ns", "p-standalone", "u1", "", 1)

	if got := pl.isPodAllowedByPlan(p); !got {
		t.Fatalf("isPodAllowedByPlan() for pinned standalone pod = %v, want true", got)
	}
}

// ----------------------------------------------------------------------
// filterNodes
// ----------------------------------------------------------------------

func TestFilterNodes_NoActivePlan(t *testing.T) {
	pl := &SharedState{}
	p := newPod("ns", "p", "u", "", 1)

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

	p := newPod("ns", "p1", "u1", "", 1)

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

	p := newPod("ns", "p-any", "u1", "", 1)

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

	p := newPod("ns", "p-unknown", "u1", "", 1)

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
	run1 := newPod("ns", "run1", "u-run1", "n1", 1)
	run2 := newPod("ns", "run2", "u-run2", "n2", 1)
	// pending pod (to be scheduled)
	pend1 := newPod("ns", "pend1", "u-pend1", "", 1)
	// deleted pod (should be ignored)
	del := newPod("ns", "del", "u-del", "n1", 1)
	del.DeletionTimestamp = &now

	pods := []*v1.Pod{run1, run2, pend1, del}

	out := &PlannerOutput{
		Placements: []PlannerPod{
			{UID: pend1.UID, Node: "n1"}, // schedule pending
			{UID: "u-unknown", Node: "n1"},
		},
		Evictions: []PlannerPod{
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
	pend := newPod("ns", "pend", "u-pend", "", 1)
	pods := []*v1.Pod{pend}

	// Eviction references a UID not present or with no node
	out := &PlannerOutput{
		Evictions: []PlannerPod{
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

	p1 := newPod("ns", "p1", "u1", "", 1)
	p2 := newPod("ns", "p2", "u2", "", 1)
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

// ----------------------------------------------------------------------
// waitPodsGone
// ---------------------------------------------------------------------

func TestWaitPodsGone_UsesHookWhenNonEmpty(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()

	p := newPod("ns", "p", "u1", "n1", 1)

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

// ----------------------------------------------------------------------
// activatePlannedPods
// ---------------------------------------------------------------------

func TestActivatePlannedPods_UsesHookWithMatchingPending(t *testing.T) {
	pl := &SharedState{}

	uidAllowed := types.UID("u-allowed")
	uidIgnored := types.UID("u-ignored")

	plan := &Plan{
		NewPlacements: []PlannerPod{
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
