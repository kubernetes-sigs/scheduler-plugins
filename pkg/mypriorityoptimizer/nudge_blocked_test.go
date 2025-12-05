// pkg/mypriorityoptimizer/nudge_blocked_test.go
// nudge_blocked_test.go

package mypriorityoptimizer

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// withNudgeInterval temporarily overrides the base interval used by nudgeBlockedLoop.
func withNudgeInterval(d time.Duration, fn func()) {
	old := nudgeBlockedBaseInterval
	nudgeBlockedBaseInterval = d
	defer func() { nudgeBlockedBaseInterval = old }()
	fn()
}

// -----------------------------------------------------------------------------
// Existing tests (unchanged)
// -----------------------------------------------------------------------------

// Ensure that when we're NOT in PerPod@PreEnqueue, the loop returns immediately
// (i.e., it doesn't start timers or block).
func TestNudgeBlockedLoop_SkipsWhenNotPerPodPreEnqueue(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	// Use a mode that is not "per_pod" so optimizePerPod() is false.
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		go func() {
			pl.nudgeBlockedLoop(ctx)
			close(done)
		}()
	})

	select {
	case <-done:
		// ok
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("nudgeBlockedLoop did not return immediately when not PerPod@PreEnqueue")
	}
}

// Ensure that in PerPod@PreEnqueue, when PluginReady=true, no active plan,
// and there are blocked pods, the loop eventually calls activatePods and
// exits cleanly on context cancellation.
func TestNudgeBlockedLoop_ActivatesBlockedPodAndHonorsContext(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	// One blocked pod.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	pl.BlockedWhileActive.AddPod(pod)

	// Fake lister that returns the pod so activatePods can resolve it.
	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod1": pod,
		},
	}

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	// Count how many times activatePodsCall is invoked.
	activateCount := 0
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		activateCount++
	}

	// Mark plugin as ready so nudgeBlockedLoop doesn't early-return.
	pl.PluginReady.Store(true)

	// Run with PerPod@PreEnqueue and a very small interval so tests are fast.
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		withNudgeInterval(2*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				pl.nudgeBlockedLoop(ctx)
				close(done)
			}()

			// Let a few ticks happen.
			time.Sleep(15 * time.Millisecond)
			cancel()

			select {
			case <-done:
				// ok
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("nudgeBlockedLoop did not exit after context cancel")
			}
		})
	})

	if activateCount == 0 {
		t.Fatalf("expected at least one activation call when plugin is ready, no active plan, and blocked pods exist")
	}

	// Because removeActivated=true in nudgeBlockedLoop, the blocked set
	// should eventually become empty.
	if remaining := pl.BlockedWhileActive.Size(); remaining != 0 {
		t.Fatalf("expected blocked set to be empty after nudge, got size=%d", remaining)
	}
}

// -----------------------------------------------------------------------------
// Additional tests to cover remaining branches
// -----------------------------------------------------------------------------

// Plugin not ready: we should reset backoff, but never activate anything.
func TestNudgeBlockedLoop_PluginNotReady_DoesNotActivate(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	// Add a blocked pod so that if we *were* ready, we'd try to activate it.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	pl.BlockedWhileActive.AddPod(pod)

	// Lister that would normally allow activation.
	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod1": pod,
		},
	}

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	activateCount := 0
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		activateCount++
	}

	// PluginReady is left at its zero value (false)
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		withNudgeInterval(2*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				pl.nudgeBlockedLoop(ctx)
				close(done)
			}()

			time.Sleep(15 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("nudgeBlockedLoop did not stop after context cancel (plugin not ready)")
			}
		})
	})

	if activateCount != 0 {
		t.Fatalf("expected no activation when plugin is not ready, got %d", activateCount)
	}
}

// Active plan in progress: we should skip nudging and reset backoff.
func TestNudgeBlockedLoop_ActivePlanSkipsActivation(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	pl.BlockedWhileActive.AddPod(pod)

	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod1": pod,
		},
	}

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	activateCount := 0
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		activateCount++
	}

	// Plugin is ready, but there is an active plan -> ap != nil branch.
	pl.PluginReady.Store(true)
	pl.ActivePlan.Store(&ActivePlan{ID: "plan-1"})

	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		withNudgeInterval(2*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				pl.nudgeBlockedLoop(ctx)
				close(done)
			}()

			time.Sleep(15 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("nudgeBlockedLoop did not stop after context cancel (active plan)")
			}
		})
	})

	if activateCount != 0 {
		t.Fatalf("expected no activation when active plan is in progress, got %d", activateCount)
	}
}

// No blocked pods: we should hit the InfoNoBlockedPods path (no activation).
func TestNudgeBlockedLoop_NoBlockedPods(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	// Plugin is ready and no active plan, but the set is empty.
	pl.PluginReady.Store(true)

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	// Lister that would be fine if there were pods.
	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     map[string]map[string]*v1.Pod{},
			errPerKey: map[string]error{},
		}
	}

	activateCount := 0
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		activateCount++
	}

	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		withNudgeInterval(2*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				pl.nudgeBlockedLoop(ctx)
				close(done)
			}()

			time.Sleep(15 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("nudgeBlockedLoop did not stop after context cancel (no blocked pods)")
			}
		})
	})

	if activateCount != 0 {
		t.Fatalf("expected no activation when blocked set is empty, got %d", activateCount)
	}
}

// len(tried) != 1 branch: simulate "attempted activation but none selected"
// by making the Pod lister always return an error, so activatePods
// finds no live Pod objects and returns an empty 'tried' slice.
func TestNudgeBlockedLoop_NoActivationResetBackoff(t *testing.T) {
	pl := &SharedState{
		BlockedWhileActive: newPodSet("blocked"),
	}

	// Blocked pod entry exists in the set.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	pl.BlockedWhileActive.AddPod(pod)

	// Lister that always errors on Get so activatePods cannot resolve the pod.
	errKey := "ns1/pod1"
	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod1": nil, // never actually returned
		},
	}
	errs := map[string]error{
		errKey: fmt.Errorf("boom"),
	}

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: errs,
		}
	}

	// We still want to verify that Activate() is never called
	// because len(tried) will be 0.
	activateCount := 0
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		activateCount++
	}

	// Plugin ready, no active plan, PerPod@PreEnqueue -> we go into
	// the main loop and hit the "attempted activation but none selected" path.
	pl.PluginReady.Store(true)

	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		withNudgeInterval(2*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				pl.nudgeBlockedLoop(ctx)
				close(done)
			}()

			time.Sleep(15 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("nudgeBlockedLoop did not stop after context cancel (no activation case)")
			}
		})
	})

	if activateCount != 0 {
		t.Fatalf("expected no activation when no pod can be selected, got %d", activateCount)
	}
}
