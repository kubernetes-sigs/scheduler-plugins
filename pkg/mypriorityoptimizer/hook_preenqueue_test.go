// hook_preenqueue_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// helper: temporarily override OptimizeMode / OptimizeHookStage using the real parsers.
func withOptimizeModeStage(modeStr, stageStr string, fn func()) {
	origMode, origStage := OptimizeMode, OptimizeHookStage
	OptimizeMode = parseOptimizeMode(modeStr)
	OptimizeHookStage = parseOptimizeHookStage(stageStr)
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
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

// In manual_all_synch with PreEnqueue hook and no active plan, we should block pods
// (to accumulate work for the solver), returning Pending.
func TestPreEnqueue_ManualPreEnqueueBlocks(t *testing.T) {
	pl := &SharedState{}
	pl.PluginReady.Store(true) // skip cache-not-ready branch

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}

	withOptimizeModeStage("manual", "preenqueue", func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != framework.Pending {
			t.Fatalf("PreEnqueue() code = %v, want %v (Pending)", st.Code(), framework.Pending)
		}
	})
}

// In a default-like configuration (all_synch@PostFilter) with no active plan,
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

	withOptimizeModeStage("periodic", "postfilter", func() {
		st := pl.PreEnqueue(context.Background(), pod)
		if st == nil {
			t.Fatalf("PreEnqueue() returned nil status")
		}
		if st.Code() != framework.Success {
			t.Fatalf("PreEnqueue() code = %v, want %v", st.Code(), framework.Success)
		}
	})
}
