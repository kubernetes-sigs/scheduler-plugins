package mypriorityoptimizer

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestActivatePods(t *testing.T) {
	//TODO
}

func TestPruneSet(t *testing.T) {
	//TODO
}

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

func TestPodSet_AddAndRemovePod(t *testing.T) {
	ps := newPodSet("blocked")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}

	// Add pod
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

	// Remove pod
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

	// Should not panic and should not change the size.
	ps.AddPod(nil)

	if got := ps.Size(); got != 0 {
		t.Fatalf("Size() after AddPod(nil) = %d, want 0", got)
	}
}

func TestPodSet_SnapshotIsCopy(t *testing.T) {
	ps := newPodSet("blocked")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	ps.AddPod(pod)

	snap := ps.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(snap))
	}

	// Modify the snapshot map and ensure the underlying set is unchanged.
	delete(snap, pod.UID)

	if got := ps.Size(); got != 1 {
		t.Fatalf("Size() after deleting from snapshot = %d, want 1", got)
	}
	snap2 := ps.Snapshot()
	if len(snap2) != 1 {
		t.Fatalf("Snapshot() length after modifying snapshot = %d, want 1", len(snap2))
	}
}
