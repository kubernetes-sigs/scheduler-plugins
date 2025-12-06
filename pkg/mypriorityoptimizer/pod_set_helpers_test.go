// pod_set_helpers_test.go
package mypriorityoptimizer

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// withPodsListerAndActivate wires podsListerForPodSets and activatePodsCall for
// the duration of fn, restoring the originals afterwards.
func withPodsListerAndActivate(
	store map[string]map[string]*v1.Pod,
	errPerKey map[string]error,
	hook func(pl *SharedState, toAct map[string]*v1.Pod),
	fn func(pl *SharedState),
) {
	oldLister := podsLister
	oldActivate := activatePods
	defer func() {
		podsLister = oldLister
		activatePods = oldActivate
	}()

	podsLister = func(*SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: errPerKey,
		}
	}
	if hook == nil {
		hook = func(*SharedState, map[string]*v1.Pod) {}
	}
	activatePods = hook

	fn(&SharedState{})
}

// -----------------------------------------------------------------------------
// activatePods
// -----------------------------------------------------------------------------

func TestActivatePods_OrdersRespectsMaxAndRemove(t *testing.T) {
	ps := newPodSet("blocked")

	pHigh := newPod("ns1", "pod-high", "uid-high", "", 30)
	pMid := newPod("ns1", "pod-mid", "uid-mid", "", 20)
	pLow := newPod("ns1", "pod-low", "uid-low", "", 10)

	ps.AddPod(pHigh)
	ps.AddPod(pMid)
	ps.AddPod(pLow)

	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod-high": pHigh,
			"pod-mid":  pMid,
			"pod-low":  pLow,
		},
	}

	var activatedKeys []string
	hook := func(_ *SharedState, toAct map[string]*v1.Pod) {
		for k := range toAct {
			activatedKeys = append(activatedKeys, k)
		}
	}

	withPodsListerAndActivate(store, nil, hook, func(pl *SharedState) {
		tried := pl.activatePods(ps, true, 2)

		// Two highest-priority pods first: high, then mid.
		if len(tried) != 2 {
			t.Fatalf("activatePods tried %d pods, want 2", len(tried))
		}
		if tried[0] != pHigh.UID || tried[1] != pMid.UID {
			t.Fatalf("tried UIDs = %v, want [%q, %q]", tried, pHigh.UID, pMid.UID)
		}

		if len(activatedKeys) != 2 {
			t.Fatalf("activatePodsCall saw %d activated keys, want 2", len(activatedKeys))
		}
		want1 := mergeNsName("ns1", "pod-high")
		want2 := mergeNsName("ns1", "pod-mid")
		gotSet := map[string]struct{}{}
		for _, k := range activatedKeys {
			gotSet[k] = struct{}{}
		}
		if _, ok := gotSet[want1]; !ok {
			t.Fatalf("expected %q to be activated", want1)
		}
		if _, ok := gotSet[want2]; !ok {
			t.Fatalf("expected %q to be activated", want2)
		}

		// removeActivated=true ⇒ only low-priority pod remains.
		if got := ps.Size(); got != 1 {
			t.Fatalf("PodSet size after activation = %d, want 1", got)
		}
		snap := ps.Snapshot()
		if _, ok := snap[pLow.UID]; !ok {
			t.Fatalf("expected low-priority pod %q to remain in PodSet", pLow.UID)
		}
	})
}

func TestActivatePods_NoRemoveKeepsInSet(t *testing.T) {
	ps := newPodSet("blocked")

	pod := newPod("ns1", "pod1", "uid-1", "", 5)
	ps.AddPod(pod)

	store := map[string]map[string]*v1.Pod{
		"ns1": {"pod1": pod},
	}

	withPodsListerAndActivate(store, nil, nil, func(pl *SharedState) {
		tried := pl.activatePods(ps, false, -1)
		if len(tried) != 1 || tried[0] != pod.UID {
			t.Fatalf("expected tried=[%q], got %v", pod.UID, tried)
		}

		// removeActivated=false => pod remains in set.
		if got := ps.Size(); got != 1 {
			t.Fatalf("PodSet size after activatePods with removeActivated=false = %d, want 1", got)
		}
	})
}

func TestActivatePods_NilSet(t *testing.T) {
	pl := &SharedState{}
	tried := pl.activatePods(nil, true, 10)
	if len(tried) != 0 {
		t.Fatalf("expected no tried UIDs for nil PodSet, got %v", tried)
	}
}

func TestActivatePods_EqualPriorityZeroTimestampSortsByName(t *testing.T) {
	ps := newPodSet("blocked")

	// Same priority, zero CreationTimestamp => fall back to name order.
	pA := newPod("ns1", "pod-a", "uid-a", "", 10)
	pB := newPod("ns1", "pod-b", "uid-b", "", 10)

	ps.AddPod(pA)
	ps.AddPod(pB)

	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod-a": pA,
			"pod-b": pB,
		},
	}

	withPodsListerAndActivate(store, nil, nil, func(pl *SharedState) {
		tried := pl.activatePods(ps, false, -1)
		if len(tried) != 2 {
			t.Fatalf("expected 2 tried pods, got %d", len(tried))
		}

		// Expect "pod-a" before "pod-b".
		if tried[0] != pA.UID || tried[1] != pB.UID {
			t.Fatalf("tried order = [%q, %q], want [%q, %q]",
				tried[0], tried[1], pA.UID, pB.UID)
		}
	})
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

	oldLister := podsLister
	defer func() { podsLister = oldLister }()
	podsLister = func(*SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: errPerKey,
		}
	}

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
