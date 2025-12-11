// hook_preenqueue_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

// -----------------------------------------------------------------------------
// PreEnqueue
// -----------------------------------------------------------------------------

// kube-system pods should always be allowed regardless of mode/plan.
func TestPreEnqueue_KubeSystemAlwaysAllowed(t *testing.T) {
	pl := &SharedState{}
	// Make sure we don't accidentally trip the "caches not warmed" branch.
	pl.PluginReady.Store(true)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sys-pod",
			Namespace: SystemNamespace,
		},
	}

	st := pl.PreEnqueue(context.Background(), pod)
	if st == nil {
		t.Fatalf("PreEnqueue() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), fwk.Success)
	}
}

// In Manual Blocking mode, PreEnqueue should block non-system pods when
// there is no active plan.
func TestPreEnqueue_ManualBlockingModeBlocks(t *testing.T) {
	pl := &SharedState{}
	pl.PluginReady.Store(true) // skip cache-not-ready branch

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}

	withMode(ModeManualBlocking, true, func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != fwk.Pending {
			t.Fatalf("PreEnqueue() code = %v, want %v (Pending)", st.Code(), fwk.Pending)
		}
	})
}

// In a default-like configuration with no active plan,
// PreEnqueue should just pass through and allow the pod.
func TestPreEnqueue_DefaultModePassThrough(t *testing.T) {
	pl := &SharedState{}
	pl.PluginReady.Store(true)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}

	withMode(ModePeriodic, true, func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != fwk.Success {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), fwk.Success)
		}
	})
}

func TestPreEnqueue_CachesNotReady_BlocksPod(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.PluginReady.Store(false)

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePeriodic, true, func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != fwk.Pending {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), fwk.Pending)
		}
		if got := pl.BlockedWhileActive.Size(); got != 1 {
			t.Fatalf("BlockedWhileActive.Size() = %d, want 1", got)
		}
	})
}

func TestPreEnqueue_ActivePlan_BlocksDisallowedPod(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.PluginReady.Store(true)
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePeriodic, true, func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != fwk.Pending {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), fwk.Pending)
		}
		if got := pl.BlockedWhileActive.Size(); got != 1 {
			t.Fatalf("BlockedWhileActive.Size() = %d, want 1", got)
		}
	})
}

func TestPreEnqueue_ActivePlan_AllowsPinnedPod(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.PluginReady.Store(true)
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": "node1"}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	withMode(ModePeriodic, true, func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != fwk.Success {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), fwk.Success)
		}
		if got := pl.BlockedWhileActive.Size(); got != 0 {
			t.Fatalf("BlockedWhileActive.Size() = %d, want 0", got)
		}
	})
}
