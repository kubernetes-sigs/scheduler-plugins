// loop_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ----------------------------------------------------------------------
// Helpers for tests
// ----------------------------------------------------------------------

func uidSet(uids ...string) map[types.UID]struct{} {
	m := make(map[types.UID]struct{}, len(uids))
	for _, u := range uids {
		m[types.UID(u)] = struct{}{}
	}
	return m
}

func int32Ptr(v int32) *int32 {
	return &v
}

func newPodWithPriority(name string, prio int32, nodeName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
			Priority: int32Ptr(prio),
		},
	}
}

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

// ----------------------------------------------------------------------
// pendingHasLowPriorityTargets
// ----------------------------------------------------------------------

func TestPendingHasLowPriorityTargets_PythonDisabledAlwaysTrue(t *testing.T) {
	origEnabled := SolverPythonEnabled
	origNum := SolverPythonNumLowerPriorities
	defer func() {
		SolverPythonEnabled = origEnabled
		SolverPythonNumLowerPriorities = origNum
	}()

	SolverPythonEnabled = false
	SolverPythonNumLowerPriorities = 10

	// No pods at all – should still return true because gating is disabled.
	if got := pendingHasLowPriorityTargets(nil); !got {
		t.Fatalf("pendingHasLowPriorityTargets(nil) with python disabled = false, want true")
	}
}

func TestPendingHasLowPriorityTargets_NoRestrictionWhenNumLowerNonPositive(t *testing.T) {
	origEnabled := SolverPythonEnabled
	origNum := SolverPythonNumLowerPriorities
	defer func() {
		SolverPythonEnabled = origEnabled
		SolverPythonNumLowerPriorities = origNum
	}()

	SolverPythonEnabled = true
	SolverPythonNumLowerPriorities = 0

	pods := []*v1.Pod{
		newPodWithPriority("p1", 5, ""), // pending
	}
	if got := pendingHasLowPriorityTargets(pods); !got {
		t.Fatalf("pendingHasLowPriorityTargets() with NumLower<=0 = false, want true")
	}
}

func TestPendingHasLowPriorityTargets_LowTierPendingExists(t *testing.T) {
	origEnabled := SolverPythonEnabled
	origNum := SolverPythonNumLowerPriorities
	defer func() {
		SolverPythonEnabled = origEnabled
		SolverPythonNumLowerPriorities = origNum
	}()

	SolverPythonEnabled = true
	SolverPythonNumLowerPriorities = 2 // keep 2 lowest priorities

	// Priorities present: 1, 5, 10
	// Lowest 2 ⇒ {1,5}. We have a pending pod at prio 5 ⇒ should be true.
	pods := []*v1.Pod{
		newPodWithPriority("run-low", 1, "n1"), // running low
		newPodWithPriority("pend-low", 5, ""),  // pending in low-tier window
		newPodWithPriority("pend-high", 10, ""),
	}

	if got := pendingHasLowPriorityTargets(pods); !got {
		t.Fatalf("pendingHasLowPriorityTargets() with a low-tier pending pod = false, want true")
	}
}

func TestPendingHasLowPriorityTargets_OnlyHighTierPending(t *testing.T) {
	origEnabled := SolverPythonEnabled
	origNum := SolverPythonNumLowerPriorities
	defer func() {
		SolverPythonEnabled = origEnabled
		SolverPythonNumLowerPriorities = origNum
	}()

	SolverPythonEnabled = true
	SolverPythonNumLowerPriorities = 1 // keep only the single lowest priority

	// Priorities present: 1, 5, 10
	// Lowest 1 ⇒ {1}. Our only pending pods are at 5 and 10 ⇒ expect false.
	pods := []*v1.Pod{
		newPodWithPriority("run-low", 1, "n1"), // running at lowest prio
		newPodWithPriority("pend-mid", 5, ""),  // pending but not in lowest tier
		newPodWithPriority("pend-high", 10, ""),
	}

	if got := pendingHasLowPriorityTargets(pods); got {
		t.Fatalf("pendingHasLowPriorityTargets() with only higher-tier pending pods = true, want false")
	}
}

func TestPendingHasLowPriorityTargets_EmptyPodsWithGating(t *testing.T) {
	origEnabled := SolverPythonEnabled
	origNum := SolverPythonNumLowerPriorities
	defer func() {
		SolverPythonEnabled = origEnabled
		SolverPythonNumLowerPriorities = origNum
	}()

	SolverPythonEnabled = true
	SolverPythonNumLowerPriorities = 3

	if got := pendingHasLowPriorityTargets(nil); got {
		t.Fatalf("pendingHasLowPriorityTargets(nil) with gating enabled = true, want false")
	}
}
