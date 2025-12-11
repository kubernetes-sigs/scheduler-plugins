// hook_prefilter_test.go
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

func TestPreFilter_ActivePlan_StandaloneAllowed_AllNodes(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": ""}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Success)
	}
	if res != nil {
		t.Fatalf("PreFilter() result = %#v, want nil when allowed on all nodes", res)
	}
}

func TestPreFilter_ActivePlan_StandalonePinned_NodeNamesReturned(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": "nodeA"}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Success)
	}
	if res == nil || res.NodeNames == nil {
		t.Fatalf("PreFilter() result = %#v, want NodeNames", res)
	}
	if !res.NodeNames.Has("nodeA") {
		t.Fatalf("PreFilter() NodeNames = %#v, want to contain nodeA", res.NodeNames.UnsortedList())
	}
}

func TestPreFilter_ActivePlan_WorkloadAllowed_SubsetOfNodes(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}

	// A controller-owned pod (ReplicaSet) should be restricted by workload quotas.
	prio := int32(0)
	pod := newPod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
	wk, ok := getTopWorkload(pod)
	if !ok {
		t.Fatalf("expected workload pod")
	}

	var c1 atomic.Int32
	c1.Store(1)
	var c0 atomic.Int32
	c0.Store(0)

	pl.ActivePlan.Store(&ActivePlan{
		ID: "ap1",
		WorkloadQuotas: WorkloadQuotasAtomics{
			wk.String(): {
				"nodeA": &c1,
				"nodeB": &c0,
			},
		},
		PlacementByName: map[string]string{},
	})

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Success {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Success)
	}
	if res == nil || res.NodeNames == nil {
		t.Fatalf("PreFilter() result = %#v, want NodeNames", res)
	}
	if !res.NodeNames.Has("nodeA") || res.NodeNames.Has("nodeB") {
		t.Fatalf("PreFilter() NodeNames = %#v, want only nodeA", res.NodeNames.UnsortedList())
	}
}

func TestPreFilter_ActivePlan_BlocksPodNotInPlan(t *testing.T) {
	pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
	pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"}}

	res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, []fwk.NodeInfo{})
	if st == nil {
		t.Fatalf("PreFilter() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("PreFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
	if res != nil {
		t.Fatalf("PreFilter() result = %#v, want nil when blocking", res)
	}
	if got := pl.BlockedWhileActive.Size(); got != 1 {
		t.Fatalf("BlockedWhileActive.Size() = %d, want 1", got)
	}
}
