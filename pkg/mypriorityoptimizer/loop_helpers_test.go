// loop_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ----------------------------------------------------------------------
// startLoops
// ----------------------------------------------------------------------

// TODO: test missing

// ----------------------------------------------------------------------
// optimizeGlobalLoop
// ----------------------------------------------------------------------

// TODO: test missing

// ----------------------------------------------------------------------
// sameUIDSet
// ----------------------------------------------------------------------

func TestSameUIDSet_NilVsNil(t *testing.T) {
	if !sameUIDSet(nil, nil) {
		t.Fatalf("sameUIDSet(nil, nil) = false, want true")
	}
}

func TestSameUIDSet_NilVsNonNil(t *testing.T) {
	a := uidSet("u1")
	if sameUIDSet(nil, a) || sameUIDSet(a, nil) {
		t.Fatalf("sameUIDSet(nil, non-nil) or reverse = true, want false")
	}
}

func TestSameUIDSet_DifferentLengths(t *testing.T) {
	a := uidSet("u1")
	b := uidSet("u1", "u2")
	if sameUIDSet(a, b) {
		t.Fatalf("sameUIDSet() with different lengths = true, want false")
	}
}

func TestSameUIDSet_SameElements(t *testing.T) {
	a := uidSet("u1", "u2")
	b := uidSet("u2", "u1")
	if !sameUIDSet(a, b) {
		t.Fatalf("sameUIDSet() with same elements = false, want true")
	}
}

func TestSameUIDSet_DifferentElements(t *testing.T) {
	a := uidSet("u1", "u2")
	b := uidSet("u1", "u3")
	if sameUIDSet(a, b) {
		t.Fatalf("sameUIDSet() with different elements = true, want false")
	}
}

// ----------------------------------------------------------------------
// cloneUIDSet
// ----------------------------------------------------------------------

func TestCloneUIDSet_Nil(t *testing.T) {
	if got := cloneUIDSet(nil); got != nil {
		t.Fatalf("cloneUIDSet(nil) = %#v, want nil", got)
	}
}

func TestCloneUIDSet_Independence(t *testing.T) {
	src := uidSet("u1", "u2")
	cloned := cloneUIDSet(src)
	if !sameUIDSet(src, cloned) {
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

// ----------------------------------------------------------------------
// isAlreadySolvedForPendingSet
// ----------------------------------------------------------------------

func TestIsAlreadySolvedForPendingSet_BestAttemptNil(t *testing.T) {
	if got := isAlreadySolvedForPendingSet(nil, nil); got {
		t.Fatalf("isAlreadySolvedForPendingSet(nil, nil) = true, want false")
	}
}

func TestIsAlreadySolvedForPendingSet_NonOptimalStatus(t *testing.T) {
	best := &SolverResult{Status: "FEASIBLE"}
	if got := isAlreadySolvedForPendingSet(ErrNoImprovingSolutionFromAnySolver, best); got {
		t.Fatalf("isAlreadySolvedForPendingSet(non-OPTIMAL) = true, want false")
	}
}

func TestIsAlreadySolvedForPendingSet_OptimalWithNoImprovementErr(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadySolvedForPendingSet(ErrNoImprovingSolutionFromAnySolver, best); !got {
		t.Fatalf("isAlreadySolvedForPendingSet(OPTIMAL, ErrNoImprovingSolutionFromAnySolver) = false, want true")
	}
}

func TestIsAlreadySolvedForPendingSet_OptimalWithNoPendingPodsErr(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadySolvedForPendingSet(ErrNoPendingPodsToSchedule, best); !got {
		t.Fatalf("isAlreadySolvedForPendingSet(OPTIMAL, ErrNoPendingPodsToSchedule) = false, want true")
	}
}

func TestIsAlreadySolvedForPendingSet_OptimalWithOtherError(t *testing.T) {
	best := &SolverResult{Status: "OPTIMAL"}
	if got := isAlreadySolvedForPendingSet(context.DeadlineExceeded, best); got {
		t.Fatalf("isAlreadySolvedForPendingSet(OPTIMAL, other error) = true, want false")
	}
}

// -----------------------------------------------------------------------------
// buildPendingSnapshot
// -----------------------------------------------------------------------------

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
