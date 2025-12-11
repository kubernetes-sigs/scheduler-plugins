// loop_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// -------------------------
// startLoops
// --------------------------

func TestStartLoops_DoesNothingWhenNotReady(t *testing.T) {
	origMode := OptimizeMode
	defer func() { OptimizeMode = origMode }()

	OptimizeMode = ModePeriodic

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pl := &SharedState{} // PluginReady default is false

	called := false

	withOptimizeLoopFunc(t,
		func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
			called = true
		},
		func() {
			pl.startLoops(ctx)
			// If it accidentally started a goroutine, this gives it a chance to run.
			time.Sleep(20 * time.Millisecond)
		},
	)

	if called {
		t.Fatalf("startLoops called optimizeBackgroundLoopFunc even though PluginReady was false")
	}
}

func TestStartLoops_LaunchesPeriodicLoopWhenModePeriodic(t *testing.T) {
	origMode := OptimizeMode
	defer func() { OptimizeMode = origMode }()

	OptimizeMode = ModePeriodic

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pl := &SharedState{}
	pl.PluginReady.Store(true)

	cfgCh := make(chan OptimizeLoopConfig, 1)

	withOptimizeLoopFunc(t,
		func(_ *SharedState, _ context.Context, cfg OptimizeLoopConfig) {
			cfgCh <- cfg
		},
		func() {
			pl.startLoops(ctx)

			select {
			case cfg := <-cfgCh:
				if cfg.Label != "PeriodicLoop" {
					t.Fatalf("expected Label=PeriodicLoop, got %q", cfg.Label)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("optimizeBackgroundLoopFunc was not called for ModePeriodic")
			}
		},
	)
}

func TestStartLoops_LaunchesInterludeLoopWhenModeInterlude(t *testing.T) {
	origMode := OptimizeMode
	defer func() { OptimizeMode = origMode }()

	OptimizeMode = ModeInterlude

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pl := &SharedState{}
	pl.PluginReady.Store(true)

	cfgCh := make(chan OptimizeLoopConfig, 1)

	withOptimizeLoopFunc(t,
		func(_ *SharedState, _ context.Context, cfg OptimizeLoopConfig) {
			cfgCh <- cfg
		},
		func() {
			pl.startLoops(ctx)

			select {
			case cfg := <-cfgCh:
				if cfg.Label != "InterludeLoop" {
					t.Fatalf("expected Label=InterludeLoop, got %q", cfg.Label)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("optimizeBackgroundLoopFunc was not called for ModeInterlude")
			}
		},
	)
}

// -------------------------
// optimizeBackgroundLoop
// --------------------------

func TestOptimizeBackgroundLoop_SkipsWhenPluginNotReady(t *testing.T) {
	pl := &SharedState{} // PluginReady default is false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0

	origSnap := buildPendingSnapshotHook
	buildPendingSnapshotHook = func(pl *SharedState) (*PendingSnapshot, error) {
		calls++
		return &PendingSnapshot{}, nil
	}
	defer func() { buildPendingSnapshotHook = origSnap }()

	cfg := OptimizeLoopConfig{
		Label:          "TestLoop",
		Interval:       10 * time.Millisecond,
		InterludeDelay: 0,
		CancelOnChange: true,
	}

	done := make(chan struct{})

	go func() {
		pl.optimizeBackgroundLoop(ctx, cfg)
		close(done)
	}()

	// Let the timer fire a few times.
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if calls != 0 {
		t.Fatalf("expected buildPendingSnapshot not to be called while PluginReady=false, got %d", calls)
	}
}

func TestOptimizeBackgroundLoop_StartsRunForStablePendingSet(t *testing.T) {
	pl := &SharedState{}
	pl.PluginReady.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pending := uidSet("p1")
	fingerprint := "fp-1"

	calls := 0

	origSnap := buildPendingSnapshotHook
	buildPendingSnapshotHook = func(_ *SharedState) (*PendingSnapshot, error) {
		calls++
		return &PendingSnapshot{
			PendingUIDs:  cloneUIDSet(pending),
			PendingCount: len(pending),
			Fingerprint:  fingerprint,
		}, nil
	}
	defer func() { buildPendingSnapshotHook = origSnap }()

	origStart := startBackgroundOptimization
	runStarted := make(chan struct{}, 1)

	startBackgroundOptimization = func(
		_ *SharedState,
		_ OptimizeLoopConfig,
		_ context.Context,
		runDone chan<- bool,
	) {
		// Mark that we started an optimization run.
		runStarted <- struct{}{}
		// Pretend we fully solved the current pending set.
		runDone <- true
		// Ensure the outer loop exits cleanly on the next select.
		cancel()
	}
	defer func() { startBackgroundOptimization = origStart }()

	cfg := OptimizeLoopConfig{
		Label:          "TestLoop",
		Interval:       5 * time.Millisecond,
		InterludeDelay: 0,
		CancelOnChange: true,
	}

	done := make(chan struct{})
	go func() {
		pl.optimizeBackgroundLoop(ctx, cfg)
		close(done)
	}()

	select {
	case <-runStarted:
		// OK: background run started.
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("background optimization run did not start in time (snapshots=%d)", calls)
	}

	<-done

	if calls < 2 {
		t.Fatalf("expected at least 2 snapshots (gating + run), got %d", calls)
	}
}

// -------------------------
// isSameUIDSet
// --------------------------

func TestIsSameUIDSet_NilVsNil(t *testing.T) {
	if !isSameUIDSet(nil, nil) {
		t.Fatalf("isSameUIDSet(nil, nil) = false, want true")
	}
}

func TestIsSameUIDSet_NilVsNonNil(t *testing.T) {
	a := uidSet("u1")
	if isSameUIDSet(nil, a) || isSameUIDSet(a, nil) {
		t.Fatalf("isSameUIDSet(nil, non-nil) or reverse = true, want false")
	}
}

func TestIsSameUIDSet_DifferentLengths(t *testing.T) {
	a := uidSet("u1")
	b := uidSet("u1", "u2")
	if isSameUIDSet(a, b) {
		t.Fatalf("isSameUIDSet() with different lengths = true, want false")
	}
}

func TestIsSameUIDSet_SameElements(t *testing.T) {
	a := uidSet("u1", "u2")
	b := uidSet("u2", "u1")
	if !isSameUIDSet(a, b) {
		t.Fatalf("isSameUIDSet() with same elements = false, want true")
	}
}

func TestIsSameUIDSet_DifferentElements(t *testing.T) {
	a := uidSet("u1", "u2")
	b := uidSet("u1", "u3")
	if isSameUIDSet(a, b) {
		t.Fatalf("isSameUIDSet() with different elements = true, want false")
	}
}

// -------------------------
// cloneUIDSet
// --------------------------

func TestCloneUIDSet_Nil(t *testing.T) {
	if got := cloneUIDSet(nil); got != nil {
		t.Fatalf("cloneUIDSet(nil) = %#v, want nil", got)
	}
}

func TestCloneUIDSet_Independence(t *testing.T) {
	src := uidSet("u1", "u2")
	cloned := cloneUIDSet(src)
	if !isSameUIDSet(src, cloned) {
		t.Fatalf("cloneUIDSet() produced different contents: src=%v cloned=%v", src, cloned)
	}
	// Mutate clone and ensure src is unaffected
	delete(cloned, types.UID("u1"))
	if _, ok := src[types.UID("u1")]; !ok {
		t.Fatalf("mutating clone mutated source: src=%v cloned=%v", src, cloned)
	}
	// Mutate src and ensure clone is unaffected
	src[types.UID("u3")] = struct{}{}
	if _, ok := cloned[types.UID("u3")]; ok {
		t.Fatalf("mutating source mutated clone: src=%v cloned=%v", src, cloned)
	}
}

// -------------------------
// isAlreadyComputedForPendingSet
// --------------------------

func TestIsAlreadyComputedForPendingSet_BestAttemptNil(t *testing.T) {
	if got := isAlreadyComputedForPendingSet(nil, nil); got {
		t.Fatalf("isAlreadyComputedForPendingSet(nil, nil) = true, want false")
	}
}

func TestIsAlreadyComputedForPendingSet_NonOptimalStatus(t *testing.T) {
	best := &SolverResult{Status: "FEASIBLE"}
	if got := isAlreadyComputedForPendingSet(ErrNoImprovingSolutionFromAnySolver, best); got {
		t.Fatalf("isAlreadyComputedForPendingSet(non-OPTIMAL) = true, want false")
	}
}

func TestIsAlreadyComputedForPendingSet_OptimalWithNoImprovementErr(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadyComputedForPendingSet(ErrNoImprovingSolutionFromAnySolver, best); !got {
		t.Fatalf("isAlreadyComputedForPendingSet(OPTIMAL, ErrNoImprovingSolutionFromAnySolver) = false, want true")
	}
}

func TestIsAlreadyComputedForPendingSet_OptimalWithNoPendingPodsErr(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadyComputedForPendingSet(ErrNoPendingPodsScheduled, best); !got {
		t.Fatalf("isAlreadyComputedForPendingSet(OPTIMAL, ErrNoPendingPodsScheduled) = false, want true")
	}
}

func TestIsAlreadyComputedForPendingSet_OptimalWithOtherError(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadyComputedForPendingSet(context.DeadlineExceeded, best); got {
		t.Fatalf("isAlreadyComputedForPendingSet(OPTIMAL, other error) = true, want false")
	}
}

// -------------------------
// buildPendingSnapshot
// --------------------------

func TestBuildPendingSnapshot(t *testing.T) {
	pl := &SharedState{}

	// One usable node
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	// One pending pod and one running pod
	pPending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-pending", Namespace: "ns", UID: types.UID("pu1")},
		Status:     v1.PodStatus{Phase: v1.PodPending},
	}
	pRunning := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-running", Namespace: "ns", UID: types.UID("pu2")},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
		Spec: v1.PodSpec{
			NodeName: "n1",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}

	// Build store to feed fakePodLister.
	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-pending": pPending,
			"p-running": pRunning,
		},
	}

	withNodeLister(&fakeNodeLister{nodes: []*v1.Node{n}}, func() {
		withPodLister(&fakePodLister{store: store}, func() {
			snap, err := pl.buildPendingSnapshot()
			if err != nil {
				t.Fatalf("buildPendingSnapshot() unexpected error: %v", err)
			}
			if snap.PendingCount != 1 {
				t.Fatalf("PendingCount = %d, want 1", snap.PendingCount)
			}
			if _, ok := snap.PendingUIDs[pPending.UID]; !ok {
				t.Fatalf("pending UID set does not contain pending pod")
			}
			if snap.Fingerprint == "" {
				t.Fatalf("Fingerprint should not be empty")
			}
			if len(snap.Pods) != 2 || len(snap.Nodes) != 1 {
				t.Fatalf("snap pods/nodes sizes wrong: pods=%d nodes=%d", len(snap.Pods), len(snap.Nodes))
			}
		})
	})
}

func TestBuildPendingSnapshot_NodesErrorPropagated(t *testing.T) {
	pl := &SharedState{}
	sentinel := errors.New("nodes boom")

	withNodeLister(&fakeNodeLister{err: sentinel}, func() {
		// Pod lister should not matter if node listing already fails,
		// but we provide a no-op lister for completeness.
		withPodLister(&fakePodLister{}, func() {
			snap, err := pl.buildPendingSnapshot()
			if snap != nil {
				t.Fatalf("expected nil snapshot on error, got %#v", snap)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("buildPendingSnapshot() err = %v, want wrapped %v", err, sentinel)
			}
		})
	})
}

func TestBuildPendingSnapshot_PodsErrorPropagated(t *testing.T) {
	pl := &SharedState{}
	sentinel := errors.New("pods boom")

	// One usable node so we get past node listing.
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	withNodeLister(&fakeNodeLister{nodes: []*v1.Node{n}}, func() {
		withPodLister(&fakePodLister{err: sentinel}, func() {
			snap, err := pl.buildPendingSnapshot()
			if snap != nil {
				t.Fatalf("expected nil snapshot on error, got %#v", snap)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("buildPendingSnapshot() err = %v, want wrapped %v", err, sentinel)
			}
		})
	})
}
