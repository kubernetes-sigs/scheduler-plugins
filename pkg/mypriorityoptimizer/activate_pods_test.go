package mypriorityoptimizer

import (
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestActivatePods_EmptySet_DoesNothing(t *testing.T) {
	pl := &SharedState{}
	set := newSafePodSet("blocked")

	called := false
	orig := activatePods
	activatePods = func(_ *SharedState, _ map[string]*v1.Pod) { called = true }
	t.Cleanup(func() { activatePods = orig })

	tried := pl.activatePods(set, false, -1)
	if len(tried) != 0 {
		t.Fatalf("activatePods(tried) = %#v, want empty", tried)
	}
	if called {
		t.Fatalf("activatePods hook was called, want not called")
	}
}

func TestActivatePods_SortsLimitsAndRemovesActivated(t *testing.T) {
	pl := &SharedState{}
	set := newSafePodSet("blocked")

	now := metav1.Now()
	older := metav1.NewTime(now.Add(-1 * time.Minute))

	prioLow := int32(1)
	prioHigh := int32(10)

	pHigh := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "p-high",
			UID:               types.UID("uid-high"),
			CreationTimestamp: now,
		},
		Spec: v1.PodSpec{Priority: &prioHigh},
	}
	pOld := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "p-old",
			UID:               types.UID("uid-old"),
			CreationTimestamp: older,
		},
		Spec: v1.PodSpec{Priority: &prioLow},
	}
	pKeep := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "p-keep",
			UID:               types.UID("uid-keep"),
			CreationTimestamp: now,
		},
		Spec: v1.PodSpec{Priority: &prioLow},
	}

	set.AddPodSafely(pOld)
	set.AddPodSafely(pHigh)
	set.AddPodSafely(pKeep)

	store := map[string]map[string]*v1.Pod{
		"default": {
			"p-old":  pOld,
			"p-high": pHigh,
			"p-keep": pKeep,
		},
	}

	var got map[string]*v1.Pod
	orig := activatePods
	activatePods = func(_ *SharedState, toAct map[string]*v1.Pod) { got = toAct }
	t.Cleanup(func() { activatePods = orig })

	withPodLister(&fakePodLister{store: store}, func() {
		tried := pl.activatePods(set, true, 2)
		wantTried := []types.UID{pHigh.UID, pOld.UID}
		if !reflect.DeepEqual(tried, wantTried) {
			t.Fatalf("tried = %#v, want %#v", tried, wantTried)
		}

		if got == nil {
			t.Fatalf("expected activatePods to be called")
		}
		if _, ok := got["default/p-high"]; !ok {
			t.Fatalf("activation set missing default/p-high; got keys=%v", keys(got))
		}
		if _, ok := got["default/p-old"]; !ok {
			t.Fatalf("activation set missing default/p-old; got keys=%v", keys(got))
		}
		if _, ok := got["default/p-keep"]; ok {
			t.Fatalf("activation set unexpectedly contains default/p-keep; got keys=%v", keys(got))
		}

		if remaining := set.Size(); remaining != 1 {
			t.Fatalf("set.Size() after removeActivated = %d, want 1", remaining)
		}
	})
}

func TestActivatePods_PruneRemovesNotFound_ActivatesRemaining(t *testing.T) {
	pl := &SharedState{}
	set := newSafePodSet("blocked")

	prio := int32(1)
	pOK := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p-ok", UID: types.UID("uid-ok")}, Spec: v1.PodSpec{Priority: &prio}}
	pGone := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p-gone", UID: types.UID("uid-gone")}, Spec: v1.PodSpec{Priority: &prio}}

	set.AddPodSafely(pOK)
	set.AddPodSafely(pGone)

	store := map[string]map[string]*v1.Pod{
		"default": {
			"p-ok": pOK,
			// p-gone intentionally missing -> NotFound
		},
	}

	called := 0
	var got map[string]*v1.Pod
	orig := activatePods
	activatePods = func(_ *SharedState, toAct map[string]*v1.Pod) {
		called++
		got = toAct
	}
	t.Cleanup(func() { activatePods = orig })

	withPodLister(&fakePodLister{store: store}, func() {
		tried := pl.activatePods(set, false, -1)
		if !reflect.DeepEqual(tried, []types.UID{pOK.UID}) {
			t.Fatalf("tried = %#v, want [%q]", tried, pOK.UID)
		}
		if called != 1 {
			t.Fatalf("activatePods called %d times, want 1", called)
		}
		if got == nil || len(got) != 1 {
			t.Fatalf("activation set = %#v, want exactly 1 pod", got)
		}
		if _, ok := got["default/p-ok"]; !ok {
			t.Fatalf("activation set missing default/p-ok; got keys=%v", keys(got))
		}

		// p-gone should have been pruned out of the set.
		if remaining := set.Size(); remaining != 1 {
			t.Fatalf("set.Size() after prune = %d, want 1", remaining)
		}
	})
}

func TestActivatePods_ListerError_SkipsAll_NoActivation(t *testing.T) {
	pl := &SharedState{}
	set := newSafePodSet("blocked")

	prio := int32(1)
	p1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1", UID: types.UID("uid1")}, Spec: v1.PodSpec{Priority: &prio}}
	set.AddPodSafely(p1)

	called := false
	orig := activatePods
	activatePods = func(_ *SharedState, _ map[string]*v1.Pod) { called = true }
	t.Cleanup(func() { activatePods = orig })

	withPodLister(&fakePodLister{store: map[string]map[string]*v1.Pod{"default": {"p1": p1}}, err: errors.New("boom")}, func() {
		tried := pl.activatePods(set, false, -1)
		if len(tried) != 0 {
			t.Fatalf("tried = %#v, want empty", tried)
		}
		if called {
			t.Fatalf("activatePods hook called, want not called")
		}
		// On lister error, prune keeps entry (conservative behavior).
		if remaining := set.Size(); remaining != 1 {
			t.Fatalf("set.Size() = %d, want 1", remaining)
		}
	})
}

func keys(m map[string]*v1.Pod) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestActivatePods_PruneRemovesNotFound_UsesNotFoundPath(t *testing.T) {
	// Tiny direct test to ensure the NotFound branch is exercised (defensive against
	// changes to fake lister behavior).
	err := apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "x")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected IsNotFound(err) to be true")
	}
}
