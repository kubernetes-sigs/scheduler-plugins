// pod_set_helpers_test.go
package mypriorityoptimizer

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// -----------------------------------------------------------------------------
// Fake PodLister / PodNamespaceLister
// -----------------------------------------------------------------------------

type fakePodLister struct {
	// store[namespace][name] = pod
	store map[string]map[string]*v1.Pod
	// errPerKey["ns/name"] = error to return from Get
	errPerKey map[string]error
}

func (f *fakePodLister) List(_ labels.Selector) ([]*v1.Pod, error) {
	var out []*v1.Pod
	for _, nsMap := range f.store {
		for _, p := range nsMap {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePodLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return &fakePodNamespaceLister{
		ns:        namespace,
		store:     f.store,
		errPerKey: f.errPerKey,
	}
}

type fakePodNamespaceLister struct {
	ns        string
	store     map[string]map[string]*v1.Pod
	errPerKey map[string]error
}

func (f *fakePodNamespaceLister) List(_ labels.Selector) ([]*v1.Pod, error) {
	var out []*v1.Pod
	if nsMap, ok := f.store[f.ns]; ok {
		for _, p := range nsMap {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePodNamespaceLister) Get(name string) (*v1.Pod, error) {
	key := f.ns + "/" + name
	if err, ok := f.errPerKey[key]; ok {
		return nil, err
	}
	nsMap := f.store[f.ns]
	if nsMap == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, name)
	}
	p, ok := nsMap[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, name)
	}
	return p, nil
}

// -----------------------------------------------------------------------------
// activatePods
// -----------------------------------------------------------------------------

func TestActivatePods_OrdersRespectsMaxAndRemove(t *testing.T) {
	pl := &SharedState{}
	ps := newPodSet("blocked")

	// Three pods with different priorities
	pHigh := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod-high",
			UID:       types.UID("uid-high"),
		},
		Spec: v1.PodSpec{},
	}
	ph := int32(30)
	pHigh.Spec.Priority = &ph

	pMid := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod-mid",
			UID:       types.UID("uid-mid"),
		},
		Spec: v1.PodSpec{},
	}
	pm := int32(20)
	pMid.Spec.Priority = &pm

	pLow := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod-low",
			UID:       types.UID("uid-low"),
		},
		Spec: v1.PodSpec{},
	}
	plow := int32(10)
	pLow.Spec.Priority = &plow

	// Add all three to the blocked set
	ps.AddPod(pHigh)
	ps.AddPod(pMid)
	ps.AddPod(pLow)

	// Fake lister returns the same pods by ns/name.
	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod-high": pHigh,
			"pod-mid":  pMid,
			"pod-low":  pLow,
		},
	}

	// Override podsListerForPodSets and activatePodsCall.
	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	var activatedKeys []string
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		for k := range toAct {
			activatedKeys = append(activatedKeys, k)
		}
	}

	// Activate at most 2 pods, and remove activated ones.
	tried := pl.activatePods(ps, true, 2)

	// We expect the two highest-priority pods to be tried first: high, then mid.
	if len(tried) != 2 {
		t.Fatalf("activatePods tried %d pods, want 2", len(tried))
	}
	if tried[0] != pHigh.UID || tried[1] != pMid.UID {
		t.Fatalf("tried UIDs = %v, want [%q, %q]", tried, pHigh.UID, pMid.UID)
	}

	// Ensure activation map contained the two higher-priority pods.
	if len(activatedKeys) != 2 {
		t.Fatalf("activatePodsCall saw %d activated keys, want 2", len(activatedKeys))
	}
	want1 := combineNsName("ns1", "pod-high")
	want2 := combineNsName("ns1", "pod-mid")
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

	// Since removeActivated=true, only the low-priority pod should remain.
	if got := ps.Size(); got != 1 {
		t.Fatalf("PodSet size after activation = %d, want 1", got)
	}
	snap := ps.Snapshot()
	if _, ok := snap[pLow.UID]; !ok {
		t.Fatalf("expected low-priority pod %q to remain in PodSet", pLow.UID)
	}
}

func TestActivatePods_NoRemoveKeepsInSet(t *testing.T) {
	pl := &SharedState{}
	ps := newPodSet("blocked")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			UID:       types.UID("uid-1"),
		},
	}
	prio := int32(5)
	pod.Spec.Priority = &prio

	ps.AddPod(pod)

	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod1": pod,
		},
	}

	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	// We don't really care about what was activated here, just that
	// removeActivated=false leaves the pod in the set.
	activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
		// no-op
	}

	tried := pl.activatePods(ps, false, -1)
	if len(tried) != 1 || tried[0] != pod.UID {
		t.Fatalf("expected tried=[%q], got %v", pod.UID, tried)
	}

	if got := ps.Size(); got != 1 {
		t.Fatalf("PodSet size after activatePods with removeActivated=false = %d, want 1", got)
	}
}

func TestActivatePods_NilSetNoPanic(t *testing.T) {
	pl := &SharedState{}

	// Should just return without panicking and without needing any lister.
	tried := pl.activatePods(nil, true, 10)
	if len(tried) != 0 {
		t.Fatalf("expected no tried UIDs for nil PodSet, got %v", tried)
	}
}

func TestActivatePods_EqualPriorityZeroTimestampSortsByName(t *testing.T) {
	pl := &SharedState{}
	ps := newPodSet("blocked")

	// Two pods with the same priority and zero CreationTimestamp.
	// Order should fall back to lexicographic name order when timestamps are zero.
	pA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod-a",
			UID:       types.UID("uid-a"),
			// CreationTimestamp left as zero value
		},
		Spec: v1.PodSpec{},
	}
	pB := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod-b",
			UID:       types.UID("uid-b"),
			// CreationTimestamp left as zero value
		},
		Spec: v1.PodSpec{},
	}

	prio := int32(10)
	pA.Spec.Priority = &prio
	pB.Spec.Priority = &prio

	ps.AddPod(pA)
	ps.AddPod(pB)

	store := map[string]map[string]*v1.Pod{
		"ns1": {
			"pod-a": pA,
			"pod-b": pB,
		},
	}

	// Override lister and activate hook.
	oldLister := podsListerForPodSets
	oldActivate := activatePodsCall
	defer func() {
		podsListerForPodSets = oldLister
		activatePodsCall = oldActivate
	}()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
		return &fakePodLister{
			store:     store,
			errPerKey: map[string]error{},
		}
	}

	// We only care about the ordering encoded in `tried`.
	activatePodsCall = func(pl *SharedState, _ map[string]*v1.Pod) {
		// no-op
	}

	tried := pl.activatePods(ps, false, -1)
	if len(tried) != 2 {
		t.Fatalf("expected 2 tried pods, got %d", len(tried))
	}

	// With equal priority and zero timestamps, ordering must fall back to name:
	// "pod-a" before "pod-b".
	if tried[0] != pA.UID || tried[1] != pB.UID {
		t.Fatalf("tried order = [%q, %q], want [%q, %q]",
			tried[0], tried[1], pA.UID, pB.UID)
	}
}

// -----------------------------------------------------------------------------
// pruneSet
// -----------------------------------------------------------------------------

func TestPruneSet_RemovesStaleAndKeepsPending(t *testing.T) {
	pl := &SharedState{}
	ps := newPodSet("blocked")

	// We create several pods that correspond to different branches in pruneSet.
	pGone := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "gone",
			UID:       types.UID("uid-gone"),
		},
	}

	pRecreatedOld := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "recreated",
			UID:       types.UID("uid-recreated-old"),
		},
	}

	now := metav1.Now()
	pTerminating := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ns",
			Name:              "term",
			UID:               types.UID("uid-term"),
			DeletionTimestamp: &now,
		},
	}

	pBound := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "bound",
			UID:       types.UID("uid-bound"),
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
		},
	}

	pErr := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "err",
			UID:       types.UID("uid-err"),
		},
	}

	pKeep := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "keep",
			UID:       types.UID("uid-keep"),
		},
		Spec: v1.PodSpec{},
	}

	// Add all to the PodSet.
	ps.AddPod(pGone)
	ps.AddPod(pRecreatedOld)
	ps.AddPod(pTerminating)
	ps.AddPod(pBound)
	ps.AddPod(pErr)
	ps.AddPod(pKeep)

	// Lister content:
	// - "gone" is NOT in store => NotFound
	// - "recreated" in store with a NEW UID (recreated)
	// - "term" with DeletionTimestamp
	// - "bound" with NodeName set
	// - "err" returns a non-NotFound error
	// - "keep" is a valid pending pod and should remain.
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
			// "gone" and "err" intentionally missing from store
		},
	}
	errPerKey := map[string]error{
		"ns/err": fmt.Errorf("some lister error"),
	}

	oldLister := podsListerForPodSets
	defer func() { podsListerForPodSets = oldLister }()

	podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
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

	// 'err' should remain because lister returned a non-NotFound error.
	if _, ok := snap[pErr.UID]; !ok {
		t.Fatalf("expected 'err' pod to be kept on lister error")
	}

	// 'keep' should remain because it is a pending, non-terminating pod.
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
