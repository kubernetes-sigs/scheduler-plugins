// hook_reserve_unreserve_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// kube-system pods should always pass Reserve without touching workload quotas.
func TestReserve_KubeSystemAlwaysAllowed(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sys-pod",
			Namespace: SystemNamespace,
		},
	}

	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Success)
	}
}

// With no active plan, non-system pods should also pass Reserve.
func TestReserve_NoActivePlan_AllowsPod(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}

	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Success)
	}
}

// Unreserve with no reservation state present should be a no-op (no panic).
func TestUnreserve_NoReservationState_NoPanic(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}
	st := framework.NewCycleState()

	// No state written → Read() will fail, and Unreserve should just log & return.
	pl.Unreserve(context.Background(), st, pod, "node1")
}

// Unreserve with a reservation state but no active plan should early-return
// after logging InfoNoActivePlan (no counter changes / panics).
func TestUnreserve_NoActivePlan_NoPanic(t *testing.T) {
	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "work-pod",
			Namespace: "default",
		},
	}
	st := framework.NewCycleState()

	// Simulate that Reserve wrote a reservation state.
	st.Write(rsReservationKey, &rsReservationState{
		key: reservationKey{
			rsKey:    "wk/ns/foo",
			nodeName: "node1",
		},
	})

	pl.Unreserve(context.Background(), st, pod, "node1")
}

// Sanity-check rsReservationState.Clone().
func TestRsReservationStateClone(t *testing.T) {
	orig := &rsReservationState{
		key: reservationKey{
			rsKey:    "wk/ns/foo",
			nodeName: "node1",
		},
	}
	clone := orig.Clone().(*rsReservationState)

	if clone == orig {
		t.Fatalf("Clone() returned same pointer, want distinct instance")
	}
	if clone.key != orig.key {
		t.Fatalf("Clone() key = %#v, want %#v", clone.key, orig.key)
	}
}
