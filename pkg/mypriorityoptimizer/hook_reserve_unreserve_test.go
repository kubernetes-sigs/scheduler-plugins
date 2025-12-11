// hook_reserve_unreserve_test.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// -------------------------
// Reserve
// --------------------------

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

// -------------------------
// Unreserve
// --------------------------

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

func TestReserve_PlacementByNamePassThrough(t *testing.T) {
	pl := &SharedState{}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": "node1"}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Success)
	}
}

func TestReserve_NotInWorkload_AllowsPod(t *testing.T) {
	pl := &SharedState{}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}, WorkloadQuotas: WorkloadQuotasAtomics{}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Success)
	}
}

func TestReserve_WorkloadNotTracked_Unschedulable(t *testing.T) {
	pl := &SharedState{}

	prio := int32(0)
	pod := newPod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
	wk, ok := getTopWorkload(pod)
	if !ok {
		t.Fatalf("expected workload pod")
	}

	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}, WorkloadQuotas: WorkloadQuotasAtomics{}})

	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
	_ = wk // keep wk in-scope to make intent explicit
}

func TestReserve_NodeNotTracked_Unschedulable(t *testing.T) {
	pl := &SharedState{}

	prio := int32(0)
	pod := newPod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
	wk, _ := getTopWorkload(pod)

	var c atomic.Int32
	c.Store(1)

	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}, WorkloadQuotas: WorkloadQuotasAtomics{wk.String(): {"node2": &c}}})

	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
}

func TestReserve_WorkloadNodeQuotaExhausted_Unschedulable(t *testing.T) {
	pl := &SharedState{}

	prio := int32(0)
	pod := newPod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
	wk, _ := getTopWorkload(pod)

	var c atomic.Int32
	c.Store(0)

	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}, WorkloadQuotas: WorkloadQuotasAtomics{wk.String(): {"node1": &c}}})

	st := pl.Reserve(context.Background(), framework.NewCycleState(), pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
}

func TestReserve_ConsumesQuota_WritesReservationState(t *testing.T) {
	pl := &SharedState{}

	prio := int32(0)
	pod := newPod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
	wk, _ := getTopWorkload(pod)

	var c atomic.Int32
	c.Store(1)

	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}, WorkloadQuotas: WorkloadQuotasAtomics{wk.String(): {"node1": &c}}})

	cs := framework.NewCycleState()
	st := pl.Reserve(context.Background(), cs, pod, "node1")
	if st == nil {
		t.Fatalf("Reserve() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("Reserve() code = %v, want %v", st.Code(), fwk.Success)
	}
	if got := c.Load(); got != 0 {
		t.Fatalf("quota after Reserve = %d, want 0", got)
	}

	data, err := cs.Read(rsReservationKey)
	if err != nil {
		t.Fatalf("CycleState.Read(rsReservationKey) err = %v", err)
	}
	rs, ok := data.(*rsReservationState)
	if !ok {
		t.Fatalf("reservation state type = %T, want *rsReservationState", data)
	}
	if rs.key.rsKey != wk.String() || rs.key.nodeName != "node1" {
		t.Fatalf("reservation key = %#v, want rsKey=%q nodeName=%q", rs.key, wk.String(), "node1")
	}
}

func TestUnreserve_ReturnsQuota_WhenActivePlanPresent(t *testing.T) {
	pl := &SharedState{}

	var c atomic.Int32
	c.Store(0)

	wkKey := "rs:default/rs1"
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", WorkloadQuotas: WorkloadQuotasAtomics{wkKey: {"node1": &c}}, PlacementByName: map[string]string{}})

	st := framework.NewCycleState()
	st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: wkKey, nodeName: "node1"}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}
	pl.Unreserve(context.Background(), st, pod, "node1")

	if got := c.Load(); got != 1 {
		t.Fatalf("quota after Unreserve = %d, want 1", got)
	}
}
