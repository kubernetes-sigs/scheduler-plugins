// plan_activation_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// -----------------------------------------------------------------------------
// Nil plan
// -----------------------------------------------------------------------------

func TestPlanActivation_NilPlan(t *testing.T) {
	pl := &SharedState{}

	err := pl.planActivation(nil, nil)
	if err != ErrNoPlanProvided {
		t.Fatalf("planActivation(nil) error = %v, want %v", err, ErrNoPlanProvided)
	}
}

// -----------------------------------------------------------------------------
// Plan with no moves/evicts → only activatePlannedPods
// -----------------------------------------------------------------------------

func TestPlanActivation_NoMovesOrEvicts_OnlyActivatePlannedPods(t *testing.T) {
	pl := &SharedState{}
	plan := &Plan{} // no moves/evicts
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"}},
	}

	origEvict := evictTargetsHook
	origWait := waitPodsGoneHook
	origActivate := activatePlannedPodsFn
	origGetPod := getPodForPlanActivation
	defer func() {
		evictTargetsHook = origEvict
		waitPodsGoneHook = origWait
		activatePlannedPodsFn = origActivate
		getPodForPlanActivation = origGetPod
	}()

	// Eviction/wait should never be called.
	evictTargetsHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		t.Fatalf("evictTargetsHook was called, but should not be for empty moves/evicts")
		return nil
	}
	waitPodsGoneHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		t.Fatalf("waitPodsGoneHook was called, but should not be for empty moves/evicts")
		return nil
	}
	getPodForPlanActivation = func(_ *SharedState, _ types.UID, _, _ string) *v1.Pod {
		t.Fatalf("getPodForPlanActivation should not be called when no moves/evicts")
		return nil
	}

	var gotPlan *Plan
	var gotPods []*v1.Pod
	activatePlannedPodsFn = func(hpl *SharedState, p *Plan, ps []*v1.Pod) {
		if hpl != pl {
			t.Fatalf("activatePlannedPodsFn received pl=%p, want %p", hpl, pl)
		}
		gotPlan = p
		gotPods = ps
	}

	if err := pl.planActivation(plan, pods); err != nil {
		t.Fatalf("planActivation() error = %v, want nil", err)
	}
	if gotPlan != plan {
		t.Fatalf("activatePlannedPodsFn plan = %#v, want %#v", gotPlan, plan)
	}
	if len(gotPods) != len(pods) {
		t.Fatalf("activatePlannedPodsFn pods len = %d, want %d", len(gotPods), len(pods))
	}
	if gotPods[0] != pods[0] {
		t.Fatalf("activatePlannedPodsFn pods[0] = %#v, want %#v", gotPods[0], pods[0])
	}
}

// -----------------------------------------------------------------------------
// Plan with moves/evicts → eviction + wait + activate
// -----------------------------------------------------------------------------

func TestPlanActivation_WithTargets_Success(t *testing.T) {
	pl := &SharedState{}

	p1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1", UID: types.UID("u1")}}
	p2 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p2", UID: types.UID("u2")}}
	pods := []*v1.Pod{p1, p2}

	plan := &Plan{
		Moves: []PlannerPod{
			{UID: p1.UID, Namespace: p1.Namespace, Name: p1.Name, OldNode: "n1", Node: "n2"},
			{UID: p1.UID, Namespace: p1.Namespace, Name: p1.Name, OldNode: "n1", Node: "n2"}, // duplicate
		},
		Evicts: []PlannerPod{
			{UID: p2.UID, Namespace: p2.Namespace, Name: p2.Name, Node: "n1"},
		},
	}

	origEvict := evictTargetsHook
	origWait := waitPodsGoneHook
	origActivate := activatePlannedPodsFn
	origGetPod := getPodForPlanActivation
	defer func() {
		evictTargetsHook = origEvict
		waitPodsGoneHook = origWait
		activatePlannedPodsFn = origActivate
		getPodForPlanActivation = origGetPod
	}()

	// Map UID -> pod to simulate getPod.
	podByUID := map[types.UID]*v1.Pod{
		p1.UID: p1,
		p2.UID: p2,
	}
	getPodForPlanActivation = func(_ *SharedState, uid types.UID, _ string, _ string) *v1.Pod {
		return podByUID[uid]
	}

	var (
		evictCtx      context.Context
		evictTargets  []*v1.Pod
		waitCtx       context.Context
		waitTargets   []*v1.Pod
		activatedPlan *Plan
		activatedPods []*v1.Pod
	)

	evictTargetsHook = func(hpl *SharedState, ctx context.Context, targets []*v1.Pod) error {
		if hpl != pl {
			t.Fatalf("evictTargetsHook pl = %p, want %p", hpl, pl)
		}
		evictCtx = ctx
		evictTargets = append([]*v1.Pod(nil), targets...)
		return nil
	}
	waitPodsGoneHook = func(hpl *SharedState, ctx context.Context, pods []*v1.Pod) error {
		if hpl != pl {
			t.Fatalf("waitPodsGoneHook pl = %p, want %p", hpl, pl)
		}
		waitCtx = ctx
		waitTargets = append([]*v1.Pod(nil), pods...)
		return nil
	}
	activatePlannedPodsFn = func(hpl *SharedState, p *Plan, ps []*v1.Pod) {
		if hpl != pl {
			t.Fatalf("activatePlannedPodsFn pl = %p, want %p", hpl, pl)
		}
		activatedPlan = p
		activatedPods = ps
	}

	if err := pl.planActivation(plan, pods); err != nil {
		t.Fatalf("planActivation() error = %v, want nil", err)
	}

	// We expect exactly the two pods, deduplicated by UID.
	if len(evictTargets) != 2 {
		t.Fatalf("evictTargets len = %d, want 2", len(evictTargets))
	}
	if len(waitTargets) != 2 {
		t.Fatalf("waitTargets len = %d, want 2", len(waitTargets))
	}

	// Order is not super important, but both p1 and p2 must be present.
	seen := map[*v1.Pod]bool{}
	for _, p := range evictTargets {
		seen[p] = true
	}
	if !seen[p1] || !seen[p2] {
		t.Fatalf("evictTargets = %#v, want both p1 and p2", evictTargets)
	}
	seen = map[*v1.Pod]bool{}
	for _, p := range waitTargets {
		seen[p] = true
	}
	if !seen[p1] || !seen[p2] {
		t.Fatalf("waitTargets = %#v, want both p1 and p2", waitTargets)
	}

	if evictCtx == nil {
		t.Fatalf("evictTargetsHook ctx is nil, want non-nil with timeout")
	}
	if _, ok := evictCtx.Deadline(); !ok {
		t.Fatalf("evictTargetsHook ctx has no deadline, want deadline (PlanOverallTimeout)")
	}
	if waitCtx == nil {
		t.Fatalf("waitPodsGoneHook ctx is nil, want non-nil with timeout")
	}

	if activatedPlan != plan {
		t.Fatalf("activated plan = %#v, want %#v", activatedPlan, plan)
	}
	if len(activatedPods) != len(pods) {
		t.Fatalf("activatedPods len = %d, want %d", len(activatedPods), len(pods))
	}
}

// -----------------------------------------------------------------------------
// Eviction error → propagated, no wait/activate
// -----------------------------------------------------------------------------

func TestPlanActivation_EvictErrorStopsFlow(t *testing.T) {
	pl := &SharedState{}

	p1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1", UID: types.UID("u1")}}
	plan := &Plan{
		Moves: []PlannerPod{
			{UID: p1.UID, Namespace: p1.Namespace, Name: p1.Name, OldNode: "n1", Node: "n2"},
		},
	}
	pods := []*v1.Pod{p1}

	origEvict := evictTargetsHook
	origWait := waitPodsGoneHook
	origActivate := activatePlannedPodsFn
	origGetPod := getPodForPlanActivation
	defer func() {
		evictTargetsHook = origEvict
		waitPodsGoneHook = origWait
		activatePlannedPodsFn = origActivate
		getPodForPlanActivation = origGetPod
	}()

	getPodForPlanActivation = func(_ *SharedState, uid types.UID, _, _ string) *v1.Pod {
		if uid == p1.UID {
			return p1
		}
		return nil
	}

	evictErr := errors.New("evict failed")
	evictCalled := false
	waitCalled := false
	activateCalled := false

	evictTargetsHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		evictCalled = true
		return evictErr
	}
	waitPodsGoneHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		waitCalled = true
		return nil
	}
	activatePlannedPodsFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) {
		activateCalled = true
	}

	err := pl.planActivation(plan, pods)
	if !errors.Is(err, evictErr) {
		t.Fatalf("planActivation() error = %v, want %v", err, evictErr)
	}
	if !evictCalled {
		t.Fatalf("evictTargetsHook was not called")
	}
	if waitCalled {
		t.Fatalf("waitPodsGoneHook was called, but should not be after evict error")
	}
	if activateCalled {
		t.Fatalf("activatePlannedPodsFn was called, but should not be after evict error")
	}
}

// -----------------------------------------------------------------------------
// waitPodsGone error → wrapped & no activate
// -----------------------------------------------------------------------------

func TestPlanActivation_WaitPodsGoneErrorStopsActivate(t *testing.T) {
	pl := &SharedState{}

	p1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1", UID: types.UID("u1")}}
	plan := &Plan{
		Evicts: []PlannerPod{
			{UID: p1.UID, Namespace: p1.Namespace, Name: p1.Name, Node: "n1"},
		},
	}
	pods := []*v1.Pod{p1}

	origEvict := evictTargetsHook
	origWait := waitPodsGoneHook
	origActivate := activatePlannedPodsFn
	origGetPod := getPodForPlanActivation
	defer func() {
		evictTargetsHook = origEvict
		waitPodsGoneHook = origWait
		activatePlannedPodsFn = origActivate
		getPodForPlanActivation = origGetPod
	}()

	getPodForPlanActivation = func(_ *SharedState, uid types.UID, _, _ string) *v1.Pod {
		if uid == p1.UID {
			return p1
		}
		return nil
	}

	evictCalled := false
	waitErr := errors.New("pods not gone yet")
	waitCalled := false
	activateCalled := false

	evictTargetsHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		evictCalled = true
		return nil
	}
	waitPodsGoneHook = func(_ *SharedState, _ context.Context, _ []*v1.Pod) error {
		waitCalled = true
		return waitErr
	}
	activatePlannedPodsFn = func(_ *SharedState, _ *Plan, _ []*v1.Pod) {
		activateCalled = true
	}

	err := pl.planActivation(plan, pods)
	if err == nil {
		t.Fatalf("planActivation() error = nil, want non-nil")
	}
	if !errors.Is(err, waitErr) {
		t.Fatalf("planActivation() error = %v, want to wrap %v", err, waitErr)
	}
	if evictCalled == false {
		t.Fatalf("evictTargetsHook was not called, want called")
	}
	if !waitCalled {
		t.Fatalf("waitPodsGoneHook was not called")
	}
	if activateCalled {
		t.Fatalf("activatePlannedPodsFn was called, but should not be after waitPodsGone error")
	}
}
