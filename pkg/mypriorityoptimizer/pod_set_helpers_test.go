// pod_set_helpers_test.go
package mypriorityoptimizer

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// -----------------------------------------------------------------------------
// newPodSet
// -----------------------------------------------------------------------------

func TestNewPodSet_Empty(t *testing.T) {
	ps := newPodSet("blocked")

	if ps == nil {
		t.Fatalf("newPodSet returned nil")
	}
	if ps.Name != "blocked" {
		t.Fatalf("newPodSet name = %q, want %q", ps.Name, "blocked")
	}

	if got := ps.Size(); got != 0 {
		t.Fatalf("newPodSet.Size() = %d, want 0", got)
	}

	snap := ps.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("newPodSet.Snapshot() length = %d, want 0", len(snap))
	}
}

// -----------------------------------------------------------------------------
// doesPodSetExist
// -----------------------------------------------------------------------------

func TestDoesPodSetExist(t *testing.T) {
	// nil set → false
	if got := doesPodSetExist(nil); got {
		t.Fatalf("doesPodSetExist(nil) = %v, want false", got)
	}

	// empty set → false
	empty := newPodSet("empty")
	if got := doesPodSetExist(empty); got {
		t.Fatalf("doesPodSetExist(empty) = %v, want false", got)
	}

	// set with one pod → true
	ps := newPodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", 5)
	ps.AddPod(pod)
	if got := doesPodSetExist(ps); !got {
		t.Fatalf("doesPodSetExist(non-empty) = %v, want true", got)
	}
}

// -----------------------------------------------------------------------------
// pruneSet
// -----------------------------------------------------------------------------

func TestPruneSet_RemovesStaleAndKeepsPending(t *testing.T) {
	pl := &SharedState{}
	ps := newPodSet("blocked")

	pGone := newPod("ns", "gone", "uid-gone", "", 0)
	pRecreatedOld := newPod("ns", "recreated", "uid-recreated-old", "", 0)

	now := metav1.Now()
	pTerminating := newPod("ns", "term", "uid-term", "", 0)
	pTerminating.DeletionTimestamp = &now

	pBound := newPod("ns", "bound", "uid-bound", "", 0)
	pBound.Spec.NodeName = "node1"

	pErr := newPod("ns", "err", "uid-err", "", 0)
	pKeep := newPod("ns", "keep", "uid-keep", "", 0)

	ps.AddPod(pGone)
	ps.AddPod(pRecreatedOld)
	ps.AddPod(pTerminating)
	ps.AddPod(pBound)
	ps.AddPod(pErr)
	ps.AddPod(pKeep)

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
		removed := pl.pruneSet(ps)
		if removed != 4 {
			t.Fatalf("pruneSet removed %d entries, want 4", removed)
		}

		snap := ps.Snapshot()
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

func TestPruneSet_EmptyOrNil(t *testing.T) {
	pl := &SharedState{}

	if removed := pl.pruneSet(nil); removed != 0 {
		t.Fatalf("pruneSet(nil) removed %d, want 0", removed)
	}

	ps := newPodSet("empty")
	if removed := pl.pruneSet(ps); removed != 0 {
		t.Fatalf("pruneSet(empty) removed %d, want 0", removed)
	}
}

// -----------------------------------------------------------------------------
// PodSet AddPod / RemovePod / Size / Snapshot
// -----------------------------------------------------------------------------

func TestPodSet_AddRemoveAndSnapshot(t *testing.T) {
	ps := newPodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", 5)

	// Add
	ps.AddPod(pod)
	if got := ps.Size(); got != 1 {
		t.Fatalf("Size() after AddPod = %d, want 1", got)
	}

	snap := ps.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() length after AddPod = %d, want 1", len(snap))
	}
	key, ok := snap[pod.UID]
	if !ok {
		t.Fatalf("Snapshot missing entry for UID %q", pod.UID)
	}
	if key.Namespace != pod.Namespace || key.Name != pod.Name || key.UID != pod.UID {
		t.Fatalf("Snapshot PodKey = %#v, want {UID:%q Namespace:%q Name:%q}",
			key, pod.UID, pod.Namespace, pod.Name)
	}

	// Remove
	ps.RemovePod(pod.UID)
	if got := ps.Size(); got != 0 {
		t.Fatalf("Size() after RemovePod = %d, want 0", got)
	}
	snap2 := ps.Snapshot()
	if len(snap2) != 0 {
		t.Fatalf("Snapshot() length after RemovePod = %d, want 0", len(snap2))
	}
}

func TestPodSet_AddPod_NilNoOp(t *testing.T) {
	ps := newPodSet("blocked")
	ps.AddPod(nil)
	if got := ps.Size(); got != 0 {
		t.Fatalf("Size() after AddPod(nil) = %d, want 0", got)
	}
}

func TestPodSet_SnapshotIsCopy(t *testing.T) {
	ps := newPodSet("blocked")
	pod := newPod("ns1", "pod1", "uid-1", "", 5)
	ps.AddPod(pod)

	snap := ps.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(snap))
	}

	// Mutating the returned map must not affect internal state.
	delete(snap, pod.UID)

	if got := ps.Size(); got != 1 {
		t.Fatalf("Size() after deleting from snapshot = %d, want 1", got)
	}
	snap2 := ps.Snapshot()
	if len(snap2) != 1 {
		t.Fatalf("Snapshot() length after modifying snapshot = %d, want 1", len(snap2))
	}
}
