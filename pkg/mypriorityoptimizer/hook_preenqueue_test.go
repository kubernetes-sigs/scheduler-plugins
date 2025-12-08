// hook_preenqueue_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// helper: temporarily override OptimizeMode using the real parsers.
func withOptimizeModeStage(modeStr string, fn func()) {
	origMode := OptimizeMode
	OptimizeMode = parseOptimizeMode(modeStr)
	defer func() {
		OptimizeMode = origMode
	}()
	fn()
}

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
	if st.Code() != framework.Success {
		t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), framework.Success)
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

	withOptimizeModeStage("manual_blocking", func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != framework.Pending {
			t.Fatalf("PreEnqueue() code = %v, want %v (Pending)", st.Code(), framework.Pending)
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

	withOptimizeModeStage("periodic", func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != framework.Success {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), framework.Success)
		}
	})
}
