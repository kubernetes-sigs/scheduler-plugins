// objects_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// -----------------------------------------------------------------------------
// nodesLister
// -----------------------------------------------------------------------------

func TestNodesLister(t *testing.T) {
	pl := &SharedState{}
	fake := &fakeNodeLister{}
	withNodeLister(fake, func() {
		got := pl.nodesLister()
		if got != fake {
			t.Fatalf("nodesLister() did not return injected lister")
		}
	})
}

// -----------------------------------------------------------------------------
// podsLister
// -----------------------------------------------------------------------------

func TestPodsLister(t *testing.T) {
	pl := &SharedState{}
	fake := &fakePodLister{}
	withPodLister(fake, func() {
		got := pl.podsLister()
		if got != fake {
			t.Fatalf("podsLister() did not return injected lister")
		}
	})
}

// -----------------------------------------------------------------------------
// getNodes
// -----------------------------------------------------------------------------

func TestGetNodes(t *testing.T) {
	nodes := []*v1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}}
	pl := &SharedState{}

	withNodeLister(&fakeNodeLister{nodes: nodes}, func() {
		got, err := pl.getNodes()
		if err != nil {
			t.Fatalf("getNodes() unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "n1" {
			t.Fatalf("getNodes() = %#v, want single node n1", got)
		}
	})

	sentinel := errors.New("boom")
	withNodeLister(&fakeNodeLister{err: sentinel}, func() {
		_, err := pl.getNodes()
		if !errors.Is(err, sentinel) {
			t.Fatalf("getNodes() error = %v, want %v", err, sentinel)
		}
	})
}

// -----------------------------------------------------------------------------
// getPods
// -----------------------------------------------------------------------------

func TestGetPods(t *testing.T) {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"}}
	pl := &SharedState{}

	// success case
	store := map[string]map[string]*v1.Pod{
		"ns": {"p1": p},
	}
	withPodLister(&fakePodLister{store: store}, func() {
		got, err := pl.getPods()
		if err != nil {
			t.Fatalf("getPods() unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "p1" || got[0].Namespace != "ns" {
			t.Fatalf("getPods() = %#v, want single pod ns/p1", got)
		}
	})

	// error case
	sentinel := errors.New("boom")
	withPodLister(&fakePodLister{err: sentinel}, func() {
		_, err := pl.getPods()
		if !errors.Is(err, sentinel) {
			t.Fatalf("getPods() error = %v, want %v", err, sentinel)
		}
	})
}

// -----------------------------------------------------------------------------
// podRef
// -----------------------------------------------------------------------------

func TestPodRef(t *testing.T) {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if got := podRef(p); got != "ns/p" {
		t.Fatalf("podRef() = %q, want %q", got, "ns/p")
	}
}

// -----------------------------------------------------------------------------
// mergeNsName
// -----------------------------------------------------------------------------

func TestMergeNsName(t *testing.T) {
	if got := mergeNsName("ns", "name"); got != "ns/name" {
		t.Fatalf("mergeNsName() = %q, want %q", got, "ns/name")
	}
}

// -----------------------------------------------------------------------------
// splitNsName
// -----------------------------------------------------------------------------

func TestSplitNsName(t *testing.T) {
	ns, name, err := splitNsName("ns/name")
	if err != nil {
		t.Fatalf("splitNsName() unexpected error: %v", err)
	}
	if ns != "ns" || name != "name" {
		t.Fatalf("splitNsName() = %q,%q, want ns,name", ns, name)
	}

	if _, _, err := splitNsName("invalid"); err == nil {
		t.Fatalf("splitNsName() expected error for invalid input")
	}
}

// -----------------------------------------------------------------------------
// countPendingPods
// -----------------------------------------------------------------------------

func TestCountPendingPods(t *testing.T) {
	if got := countPendingPods(nil); got != 0 {
		t.Fatalf("countPendingPods(nil) = %d, want 0", got)
	}

	now := metav1.NewTime(time.Now())

	pods := []*v1.Pod{
		nil, // ignored
		{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "ns"},
			Spec: v1.PodSpec{NodeName: "n1"}}, // bound
		{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "ns"},
			Spec: v1.PodSpec{NodeName: ""}}, // pending
		{ObjectMeta: metav1.ObjectMeta{Name: "terminating", Namespace: "ns",
			DeletionTimestamp: &now},
			Spec: v1.PodSpec{NodeName: ""}}, // ignored due to deletion
	}

	if got := countPendingPods(pods); got != 1 {
		t.Fatalf("countPendingPods() = %d, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// evictPod
// -----------------------------------------------------------------------------

func TestEvictPod_Success(t *testing.T) {
	pl := &SharedState{}
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "ns",
			UID:       types.UID("uid-1"),
		},
	}

	var capturedEv *policyv1.Eviction
	withEvictHook(
		func(_ *SharedState, _ context.Context, pod *v1.Pod, ev *policyv1.Eviction) error {
			if pod != p {
				t.Fatalf("evictPodFor called with unexpected pod: %#v", pod)
			}
			capturedEv = ev
			return nil
		},
		func() {
			if err := pl.evictPod(context.Background(), p); err != nil {
				t.Fatalf("evictPod() unexpected error: %v", err)
			}
		},
	)

	if capturedEv == nil {
		t.Fatalf("evictPod did not pass eviction to hook")
	}
	if capturedEv.Name != "p" || capturedEv.Namespace != "ns" || capturedEv.UID != p.UID {
		t.Fatalf("eviction metadata mismatch: %#v", capturedEv.ObjectMeta)
	}
	if capturedEv.DeleteOptions == nil || capturedEv.DeleteOptions.GracePeriodSeconds == nil {
		t.Fatalf("eviction DeleteOptions not set")
	}
	if *capturedEv.DeleteOptions.GracePeriodSeconds != 0 {
		t.Fatalf("GracePeriodSeconds = %d, want 0", *capturedEv.DeleteOptions.GracePeriodSeconds)
	}
	if capturedEv.DeleteOptions.Preconditions == nil || capturedEv.DeleteOptions.Preconditions.UID == nil {
		t.Fatalf("Preconditions UID not set")
	}
	if *capturedEv.DeleteOptions.Preconditions.UID != p.UID {
		t.Fatalf("Preconditions UID = %q, want %q", *capturedEv.DeleteOptions.Preconditions.UID, p.UID)
	}
}

func TestEvictPod_Error(t *testing.T) {
	pl := &SharedState{}
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	sentinel := errors.New("boom")

	withEvictHook(
		func(_ *SharedState, _ context.Context, _ *v1.Pod, _ *policyv1.Eviction) error {
			return sentinel
		},
		func() {
			err := pl.evictPod(context.Background(), p)
			if !errors.Is(err, sentinel) {
				t.Fatalf("evictPod() error = %v, want %v", err, sentinel)
			}
		},
	)
}

// -----------------------------------------------------------------------------
// recreateStandalonePod
// -----------------------------------------------------------------------------

func TestRecreateStandalonePod_Success(t *testing.T) {
	pl := &SharedState{}
	orig := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "p",
			Namespace:       "ns",
			UID:             types.UID("uid-1"),
			GenerateName:    "gen-",
			ResourceVersion: "123",
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}

	nodeSelector := map[string]string{"foo": "bar"}

	orig.Spec.NodeName = "node1"
	orig.Spec.NodeSelector = nodeSelector

	var created *v1.Pod
	withCreateHook(
		func(_ *SharedState, _ context.Context, pod *v1.Pod) (*v1.Pod, error) {
			created = pod
			// Simulate API return value (usually server-side fields changed)
			return pod, nil
		},
		func() {
			if err := pl.recreateStandalonePod(context.Background(), orig, "ignored"); err != nil {
				t.Fatalf("recreateStandalonePod() unexpected error: %v", err)
			}
		},
	)

	if created == nil {
		t.Fatalf("recreateStandalonePod did not call create hook")
	}
	if created.Name != orig.Name || created.Namespace != orig.Namespace {
		t.Fatalf("created pod name/ns mismatch: %#v", created.ObjectMeta)
	}
	if created.UID != "" {
		t.Fatalf("created pod UID = %q, want empty", created.UID)
	}
	if created.GenerateName != "" {
		t.Fatalf("created pod GenerateName = %q, want empty", created.GenerateName)
	}
	if created.ResourceVersion != "" {
		t.Fatalf("created pod ResourceVersion = %q, want empty", created.ResourceVersion)
	}
	if created.Spec.NodeName != "" {
		t.Fatalf("created pod NodeName = %q, want empty", created.Spec.NodeName)
	}
	if len(created.Spec.NodeSelector) != 0 {
		t.Fatalf("created pod NodeSelector = %#v, want empty map", created.Spec.NodeSelector)
	}
	if created.Status.Phase != "" {
		t.Fatalf("created pod Status.Phase = %q, want empty", created.Status.Phase)
	}
	// Ensure original pod not mutated.
	if orig.Spec.NodeName != "node1" {
		t.Fatalf("orig pod NodeName mutated: %q", orig.Spec.NodeName)
	}
	if len(orig.Spec.NodeSelector) != 1 || orig.Spec.NodeSelector["foo"] != "bar" {
		t.Fatalf("orig pod NodeSelector mutated: %#v", orig.Spec.NodeSelector)
	}
}

func TestRecreateStandalonePod_Error(t *testing.T) {
	pl := &SharedState{}
	orig := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "ns",
		},
	}
	sentinel := errors.New("boom")

	withCreateHook(
		func(_ *SharedState, _ context.Context, _ *v1.Pod) (*v1.Pod, error) {
			return nil, sentinel
		},
		func() {
			err := pl.recreateStandalonePod(context.Background(), orig, "ignored")
			if err == nil {
				t.Fatalf("recreateStandalonePod() expected error")
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("recreateStandalonePod() error = %v, want wrapped %v", err, sentinel)
			}
		},
	)
}

// -----------------------------------------------------------------------------
// getPodCPURequest
// -----------------------------------------------------------------------------

func TestGetPodCPURequest(t *testing.T) {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("250m"),
						},
					},
				},
			},
		},
	}
	if got := getPodCPURequest(p); got != 350 {
		t.Fatalf("getPodCPURequest() = %d, want 350", got)
	}

	pEmpty := &v1.Pod{}
	if got := getPodCPURequest(pEmpty); got != 0 {
		t.Fatalf("getPodCPURequest(empty) = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// getPodMemoryRequest
// -----------------------------------------------------------------------------

func TestGetPodMemoryRequest(t *testing.T) {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}

	// MustParse(...) returns a non-addressable Quantity, so we must store it
	// in a variable before calling the pointer method Value().
	q64 := resource.MustParse("64Mi")
	q128 := resource.MustParse("128Mi")
	want := q64.Value() + q128.Value()

	if got := getPodMemoryRequest(p); got != want {
		t.Fatalf("getPodMemoryRequest() = %d, want %d", got, want)
	}

	pEmpty := &v1.Pod{}
	if got := getPodMemoryRequest(pEmpty); got != 0 {
		t.Fatalf("getPodMemoryRequest(empty) = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// getPodPriority
// -----------------------------------------------------------------------------

func TestGetPodPriority(t *testing.T) {
	pval := int32(10)
	withPrio := &v1.Pod{Spec: v1.PodSpec{Priority: &pval}}
	if got := getPodPriority(withPrio); got != 10 {
		t.Fatalf("getPodPriority(with priority) = %d, want 10", got)
	}

	without := &v1.Pod{}
	if got := getPodPriority(without); got != 0 {
		t.Fatalf("getPodPriority(nil) = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// isNodeControlPlane
// -----------------------------------------------------------------------------

func TestIsNodeControlPlane(t *testing.T) {
	n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{}}}
	if isNodeControlPlane(n) {
		t.Fatalf("worker node should not be control plane")
	}

	n1 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
		"node-role.kubernetes.io/control-plane": "true",
	}}}
	if !isNodeControlPlane(n1) {
		t.Fatalf("node with control-plane label should be control plane")
	}

	n2 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{
		"node-role.kubernetes.io/master": "true",
	}}}
	if !isNodeControlPlane(n2) {
		t.Fatalf("node with master label should be control plane")
	}

	n3 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-plane"}}
	if !isNodeControlPlane(n3) {
		t.Fatalf("node named control-plane should be control plane")
	}

	n4 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "kind-control-plane"}}
	if !isNodeControlPlane(n4) {
		t.Fatalf("node named kind-control-plane should be control plane")
	}
}

// -----------------------------------------------------------------------------
// isNodeReady
// -----------------------------------------------------------------------------

func TestNodeReady(t *testing.T) {
	n := &v1.Node{}
	if isNodeReady(n) {
		t.Fatalf("node with no conditions should not be ready")
	}

	n.Status.Conditions = []v1.NodeCondition{
		{Type: v1.NodeReady, Status: v1.ConditionFalse},
	}
	if isNodeReady(n) {
		t.Fatalf("node with NodeReady=False should not be ready")
	}

	n.Status.Conditions[0].Status = v1.ConditionTrue
	if !isNodeReady(n) {
		t.Fatalf("node with NodeReady=True should be ready")
	}
}

// -----------------------------------------------------------------------------
// isNodeNoScheduleConditionTainted
// -----------------------------------------------------------------------------

func TestIsNodeNoScheduleConditionTainted(t *testing.T) {
	n := &v1.Node{}
	if isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("empty node should not have NoScheduleCondTaint")
	}

	n.Spec.Taints = []v1.Taint{
		{
			Key:    "node.kubernetes.io/not-ready",
			Effect: v1.TaintEffectNoSchedule,
		},
	}
	if !isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("node with not-ready NoSchedule taint should return true")
	}

	n.Spec.Taints = []v1.Taint{
		{
			Key:    "node.kubernetes.io/unreachable",
			Effect: v1.TaintEffectNoSchedule,
		},
	}
	if !isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("node with unreachable NoSchedule taint should return true")
	}

	n.Spec.Taints = []v1.Taint{
		{
			Key:    "node.kubernetes.io/not-ready",
			Effect: v1.TaintEffectPreferNoSchedule,
		},
	}
	if isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("PreferNoSchedule should not count as NoScheduleCondTaint")
	}
}

// -----------------------------------------------------------------------------
// isNodeAllocatable
// -----------------------------------------------------------------------------

func TestIsNodeAllocatable(t *testing.T) {
	n := &v1.Node{
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{},
		},
	}
	if isNodeAllocatable(n) {
		t.Fatalf("node with empty allocatable should not be allocatable")
	}

	n.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("1000m"),
		v1.ResourceMemory: resource.MustParse("1Gi"),
	}
	if !isNodeAllocatable(n) {
		t.Fatalf("node with allocatable resources should be allocatable")
	}
}

// -----------------------------------------------------------------------------
// isNodeUsable
// -----------------------------------------------------------------------------

func TestIsNodeUsable(t *testing.T) {
	if isNodeUsable(nil) {
		t.Fatalf("nil node should not be usable")
	}

	// Control plane
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("control plane node should not be usable")
	}

	// Unschedulable worker
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       v1.NodeSpec{Unschedulable: true},
	}
	if isNodeUsable(n) {
		t.Fatalf("unschedulable node should not be usable")
	}

	// Not ready
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("not-ready node should not be usable")
	}

	// Ready but tainted
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n3"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
		Spec: v1.NodeSpec{
			Taints: []v1.Taint{
				{Key: "node.kubernetes.io/not-ready", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("tainted node should not be usable")
	}

	// Ready, no taints but zero allocatable
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n4"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("0m"),
				v1.ResourceMemory: resource.MustParse("0"),
			},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("node with zero allocatable should not be usable")
	}

	// Fully usable
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n5"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	if !isNodeUsable(n) {
		t.Fatalf("expected usable node to be usable")
	}
}

// -----------------------------------------------------------------------------
// podsByUID
// -----------------------------------------------------------------------------

func TestPodsByUID(t *testing.T) {
	now := metav1.NewTime(time.Now())
	p1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: types.UID("u1")}}
	p2 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: types.UID("u2"), DeletionTimestamp: &now}}
	p3 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3", UID: types.UID("u1")}} // overwrites u1

	m := podsByUID([]*v1.Pod{p1, p2, nil, p3})
	if len(m) != 1 {
		t.Fatalf("podsByUID len = %d, want 1", len(m))
	}
	if got, ok := m[types.UID("u1")]; !ok || got.Name != "p3" {
		t.Fatalf("podsByUID[u1] = %#v (ok=%v), want p3", got, ok)
	}
}

// -----------------------------------------------------------------------------
// clusterFingerprint
// -----------------------------------------------------------------------------

func TestClusterFingerprint(t *testing.T) {
	n1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	n2 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2000m"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
	}

	p1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", UID: types.UID("u1")},
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
	// Pending pod should be ignored in fingerprint.
	pPending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns", UID: types.UID("u2")},
		Spec: v1.PodSpec{
			NodeName: "",
		},
	}

	// Order 1
	fp1 := clusterFingerprint([]*v1.Node{n2, n1}, []*v1.Pod{pPending, p1})
	// Order 2 (shuffled)
	fp2 := clusterFingerprint([]*v1.Node{n1, n2}, []*v1.Pod{p1, pPending})

	if fp1 != fp2 {
		t.Fatalf("clusterFingerprint not deterministic: fp1=%q fp2=%q", fp1, fp2)
	}

	// Change a running pod's CPU → fingerprint should change.
	p1diff := p1.DeepCopy()
	p1diff.Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("200m")
	fp3 := clusterFingerprint([]*v1.Node{n1, n2}, []*v1.Pod{p1diff})

	if fp1 == fp3 {
		t.Fatalf("clusterFingerprint should change when running pod CPU changes")
	}

	// Change only pending pods → fingerprint should stay the same.
	fp4 := clusterFingerprint([]*v1.Node{n1, n2}, []*v1.Pod{p1, pPending, {
		ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "ns", UID: types.UID("u3")},
	}})
	if fp1 != fp4 {
		t.Fatalf("clusterFingerprint should ignore changes in pending pods")
	}
}

// -----------------------------------------------------------------------------
// isPreemptor
// -----------------------------------------------------------------------------

func TestIsPreemptor(t *testing.T) {
	u1 := types.UID("u1")
	u2 := types.UID("u2")

	if !isPreemptor(u1, u1) {
		t.Fatalf("expected isPreemptor(u1,u1) to be true")
	}
	if isPreemptor(u1, u2) {
		t.Fatalf("expected isPreemptor(u1,u2) to be false")
	}
	if isPreemptor("", u2) {
		t.Fatalf("expected isPreemptor('',u2) to be false")
	}
	if isPreemptor(u1, "") {
		t.Fatalf("expected isPreemptor(u1,'') to be false")
	}
}

// -----------------------------------------------------------------------------
// WorkloadKey.String
// -----------------------------------------------------------------------------

func TestString(t *testing.T) {
	wk := WorkloadKey{Kind: wkReplicaSet, Namespace: "ns", Name: "foo"}
	if got := wk.String(); got != "rs:ns/foo" {
		t.Fatalf("ReplicaSet String() = %q, want %q", got, "rs:ns/foo")
	}

	wk = WorkloadKey{Kind: wkStatefulSet, Namespace: "ns", Name: "foo"}
	if got := wk.String(); got != "ss:ns/foo" {
		t.Fatalf("StatefulSet String() = %q, want %q", got, "ss:ns/foo")
	}

	wk = WorkloadKey{Kind: wkDaemonSet, Namespace: "ns", Name: "foo"}
	if got := wk.String(); got != "ds:ns/foo" {
		t.Fatalf("DaemonSet String() = %q, want %q", got, "ds:ns/foo")
	}

	wk = WorkloadKey{Kind: wkJob, Namespace: "ns", Name: "foo"}
	if got := wk.String(); got != "job:ns/foo" {
		t.Fatalf("Job String() = %q, want %q", got, "job:ns/foo")
	}

	wk = WorkloadKey{Kind: WorkloadKind(999), Namespace: "ns", Name: "foo"}
	if got := wk.String(); got != "ns/foo" {
		t.Fatalf("Unknown kind String() = %q, want %q", got, "ns/foo")
	}
}

// -----------------------------------------------------------------------------
// topWorkload
// -----------------------------------------------------------------------------

func TestTopWorkload(t *testing.T) {
	// ReplicaSet controller
	ctrlTrue := true
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "rs1", Controller: &ctrlTrue},
			},
		},
	}
	wk, ok := topWorkload(p)
	if !ok {
		t.Fatalf("expected topWorkload to return true for RS controller")
	}
	if wk.Kind != wkReplicaSet || wk.Namespace != "ns" || wk.Name != "rs1" {
		t.Fatalf("topWorkload(RS) = %#v, want rs:ns/rs1", wk)
	}

	// StatefulSet controller
	p = p.DeepCopy()
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "ss1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkStatefulSet || wk.Name != "ss1" {
		t.Fatalf("topWorkload(SS) = %#v (ok=%v), want ss:ns/ss1", wk, ok)
	}

	// DaemonSet controller
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkDaemonSet || wk.Name != "ds1" {
		t.Fatalf("topWorkload(DS) = %#v (ok=%v), want ds:ns/ds1", wk, ok)
	}

	// Job controller
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "job1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkJob || wk.Name != "job1" {
		t.Fatalf("topWorkload(Job) = %#v (ok=%v), want job:ns/job1", wk, ok)
	}

	// No controller
	p.OwnerReferences = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "rs1", Controller: nil},
	}
	wk, ok = topWorkload(p)
	if ok {
		t.Fatalf("expected topWorkload to return false when no Controller=true owner exists, got %#v", wk)
	}
}
