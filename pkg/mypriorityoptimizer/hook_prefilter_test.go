// hook_prefilter_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// -----------------------------------------------------------------------------
// PreFilter
// -----------------------------------------------------------------------------

// kube-system pods should always be allowed and not constrained by any active plan.
func TestPreFilter_KubeSystemAlwaysAllowed(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sys-pod",
			Namespace: SystemNamespace,
		},
	}

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Success)
	}
	if res != nil {
		t.Fatalf("PreFilter() result = %#v, want nil for kube-system pod", res)
	}
}

// When there is no active plan, regular pods should pass through PreFilter unmodified.
func TestPreFilter_NoActivePlan_AllowsPod(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Success)
	}
	if res != nil {
		t.Fatalf("PreFilter() result = %#v, want nil when no active plan", res)
	}
}

// PreFilterExtensions should be nil (no additional callbacks).
func TestPreFilterExtensions_IsNil(t *testing.T) {
	pl := &SharedState{}
	if ext := pl.PreFilterExtensions(); ext != nil {
		t.Fatalf("PreFilterExtensions() = %#v, want nil", ext)
	}
}
