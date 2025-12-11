// pod_set_helpers_test.go
package mypriorityoptimizer

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// -------------------------
// newSafePodSet
// --------------------------

func TestNewSafePodSet_Empty(t *testing.T) {
	ps := newSafePodSet("blocked")

	if ps == nil {
		t.Fatalf("newSafePodSet returned nil")
	}
	if ps.Name != "blocked" {
		t.Fatalf("newSafePodSet name = %q, want %q", ps.Name, "blocked")
	}

	if got := ps.Size(); got != 0 {
		t.Fatalf("newSafePodSet.Size() = %d, want 0", got)
	}

	snap := ps.SnapshotSafely()
	if len(snap) != 0 {
		t.Fatalf("newSafePodSet.SnapshotSafely() length = %d, want 0", len(snap))
	}
}

// -------------------------
// doesSafePodSetExist
// --------------------------

func TestDoesSafePodSetExist(t *testing.T) {
	// nil set → false
	if got := doesSafePodSetExist(nil); got {
		t.Fatalf("doesSafePodSetExist(nil) = %v, want false", got)
	}

	// empty set → false
	empty := newSafePodSet("empty")
	if got := doesSafePodSetExist(empty); got {
		t.Fatalf("doesSafePodSetExist(empty) = %v, want false", got)
	}

	// set with one pod → true
	ps := newSafePodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", "", "", 5)
	ps.AddPodSafely(pod)
	if got := doesSafePodSetExist(ps); !got {
		t.Fatalf("doesSafePodSetExist(non-empty) = %v, want true", got)
	}
}

// -------------------------
// pruneSafePodSet
// --------------------------

func TestPruneSet_RemovesStaleAndKeepsPending(t *testing.T) {
	pl := &SharedState{}
	ps := newSafePodSet("blocked")

	pGone := newPod("ns", "gone", "uid-gone", "", "", "", 0)
	pRecreatedOld := newPod("ns", "recreated", "uid-recreated-old", "", "", "", 0)

	now := metav1.Now()
	pTerminating := newPod("ns", "term", "uid-term", "", "", "", 0)
	pTerminating.DeletionTimestamp = &now

	pBound := newPod("ns", "bound", "uid-bound", "", "", "", 0)
	pBound.Spec.NodeName = "node1"

	pErr := newPod("ns", "err", "uid-err", "", "", "", 0)
	pKeep := newPod("ns", "keep", "uid-keep", "", "", "", 0)

	ps.AddPodSafely(pGone)
	ps.AddPodSafely(pRecreatedOld)
	ps.AddPodSafely(pTerminating)
	ps.AddPodSafely(pBound)
	ps.AddPodSafely(pErr)
	ps.AddPodSafely(pKeep)

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"recreated": {
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "recreated",
					UID:       types.UID("uid-recreated-new"),
				},
			},
			"term":  pTerminating,
			"bound": pBound,
			"keep":  pKeep,
		},
	}
	errPerKey := map[string]error{
		"ns/err": fmt.Errorf("some lister error"),
	}

	withPodLister(&fakePodLister{
		store:     store,
		errPerKey: errPerKey,
	}, func() {
		removed := pl.pruneSafePodSet(ps)
		if removed != 4 {
			t.Fatalf("pruneSet removed %d entries, want 4", removed)
		}

		snap := ps.SnapshotSafely()
		if _, ok := snap[pGone.UID]; ok {
			t.Fatalf("expected 'gone' pod to be pruned")
		}
		if _, ok := snap[pRecreatedOld.UID]; ok {
			t.Fatalf("expected 'recreated' old UID to be pruned")
		}
		if _, ok := snap[pTerminating.UID]; ok {
			t.Fatalf("expected terminating pod to be pruned")
		}
		if _, ok := snap[pBound.UID]; ok {
			t.Fatalf("expected bound pod to be pruned")
		}

		// 'err' stays on non-NotFound error.
		if _, ok := snap[pErr.UID]; !ok {
			t.Fatalf("expected 'err' pod to be kept on lister error")
		}
		// 'keep' stays as valid pending pod.
		if _, ok := snap[pKeep.UID]; !ok {
			t.Fatalf("expected 'keep' pod to remain pending")
		}
	})
}

func TestPruneSafePodSet_EmptyOrNil(t *testing.T) {
	pl := &SharedState{}

	if removed := pl.pruneSafePodSet(nil); removed != 0 {
		t.Fatalf("pruneSafePodSet(nil) removed %d, want 0", removed)
	}

	ps := newSafePodSet("empty")
	if removed := pl.pruneSafePodSet(ps); removed != 0 {
		t.Fatalf("pruneSafePodSet(empty) removed %d, want 0", removed)
	}
}

// -------------------------
// SafePodSet AddPod / RemovePod / Size / Snapshot
// --------------------------

func TestSafePodSet_AddRemoveAndSnapshot(t *testing.T) {
	ps := newSafePodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", "", "", 5)

	// Add
	ps.AddPodSafely(pod)
	if got := ps.Size(); got != 1 {
		t.Fatalf("Size() after AddPod = %d, want 1", got)
	}

	snap := ps.SnapshotSafely()
	if len(snap) != 1 {
		t.Fatalf("SnapshotSafely() length after AddPod = %d, want 1", len(snap))
	}
	key, ok := snap[pod.UID]
	if !ok {
		t.Fatalf("SnapshotSafely missing entry for UID %q", pod.UID)
	}
	if key.Namespace != pod.Namespace || key.Name != pod.Name || key.UID != pod.UID {
		t.Fatalf("SnapshotSafely PodKey = %#v, want {UID:%q Namespace:%q Name:%q}",
			key, pod.UID, pod.Namespace, pod.Name)
	}

	// Remove
	ps.RemovePodSafely(pod.UID)
	if got := ps.Size(); got != 0 {
		t.Fatalf("Size() after RemovePod = %d, want 0", got)
	}
	snap2 := ps.SnapshotSafely()
	if len(snap2) != 0 {
		t.Fatalf("SnapshotSafely() length after RemovePod = %d, want 0", len(snap2))
	}
}

func TestSafePodSet_AddPod_NilNoOp(t *testing.T) {
	ps := newSafePodSet("blocked")
	ps.AddPodSafely(nil)
	if got := ps.Size(); got != 0 {
		t.Fatalf("Size() after AddPod(nil) = %d, want 0", got)
	}
}

func TestSafePodSet_SnapshotIsCopy(t *testing.T) {
	ps := newSafePodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", "", "", 5)
	ps.AddPodSafely(pod)

	snap := ps.SnapshotSafely()
	if len(snap) != 1 {
		t.Fatalf("SnapshotSafely() length = %d, want 1", len(snap))
	}

	// Mutating the returned map must not affect internal state.
	delete(snap, pod.UID)

	if got := ps.Size(); got != 1 {
		t.Fatalf("Size() after deleting from snapshot = %d, want 1", got)
	}
	snap2 := ps.SnapshotSafely()
	if len(snap2) != 1 {
		t.Fatalf("SnapshotSafely() length after modifying snapshot = %d, want 1", len(snap2))
	}
}
