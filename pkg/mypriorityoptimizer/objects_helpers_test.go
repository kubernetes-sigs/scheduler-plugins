// pkg/mypriorityoptimizer/objects_helpers_test.go
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

//
// -----------------------------------------------------------------------------
// nodesLister / podsLister hooks + getNodes/getPods
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

func TestGetPods(t *testing.T) {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"}}
	pl := &SharedState{}

	// success
	store := map[string]map[string]*v1.Pod{"ns": {"p1": p}}
	withPodLister(&fakePodLister{store: store}, func() {
		got, err := pl.getPods()
		if err != nil {
			t.Fatalf("getPods() unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "p1" || got[0].Namespace != "ns" {
			t.Fatalf("getPods() = %#v, want single pod ns/p1", got)
		}
	})

	// error
	sentinel := errors.New("boom")
	withPodLister(&fakePodLister{err: sentinel}, func() {
		_, err := pl.getPods()
		if !errors.Is(err, sentinel) {
			t.Fatalf("getPods() error = %v, want %v", err, sentinel)
		}
	})
}

//
// -----------------------------------------------------------------------------
// podRef / mergeNsName / splitNsName
// -----------------------------------------------------------------------------

func TestPodRef(t *testing.T) {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if got := podRef(p); got != "ns/p" {
		t.Fatalf("podRef() = %q, want %q", got, "ns/p")
	}
}

func TestMergeNsName(t *testing.T) {
	if got := mergeNsName("ns", "name"); got != "ns/name" {
		t.Fatalf("mergeNsName() = %q, want %q", got, "ns/name")
	}
}

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

//
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
		{ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating",
			Namespace:         "ns",
			DeletionTimestamp: &now,
		},
			Spec: v1.PodSpec{NodeName: ""}}, // ignored
	}
	if got := countPendingPods(pods); got != 1 {
		t.Fatalf("countPendingPods() = %d, want 1", got)
	}
}

//
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

//
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
	// original must not be mutated
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

//
// -----------------------------------------------------------------------------
// Node resource helpers
// -----------------------------------------------------------------------------

func TestGetNodeCPUAllocatable(t *testing.T) {
	n := &v1.Node{
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("1500m"),
			},
		},
	}
	if got := getNodeCPUAllocatable(n); got != 1500 {
		t.Fatalf("getNodeCPUAllocatable() = %d, want 1500", got)
	}
}

func TestGetNodeMemoryAllocatable(t *testing.T) {
	q := resource.MustParse("2Gi")
	n := &v1.Node{
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceMemory: q,
			},
		},
	}
	want := q.Value()
	if got := getNodeMemoryAllocatable(n); got != want {
		t.Fatalf("getNodeMemoryAllocatable() = %d, want %d", got, want)
	}
}

//
// -----------------------------------------------------------------------------
// isNodeControlPlane / getNodeConditions / isNodeReady / getNodeTaints /
// isNodeNoScheduleConditionTainted / isNodeAllocatable / isNodeUnschedulable /
// isNodeUsable
// -----------------------------------------------------------------------------

func TestIsNodeControlPlane(t *testing.T) {
	n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{}}}
	if isNodeControlPlane(n) {
		t.Fatalf("worker node should not be control plane")
	}

	n1 := &v1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "n1",
		Labels: map[string]string{
			"node-role.kubernetes.io/control-plane": "true",
		}}}
	if !isNodeControlPlane(n1) {
		t.Fatalf("node with control-plane label should be control plane")
	}

	n2 := &v1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "n2",
		Labels: map[string]string{
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

func TestGetNodeConditions(t *testing.T) {
	conds := []v1.NodeCondition{
		{Type: v1.NodeReady, Status: v1.ConditionTrue},
	}
	n := &v1.Node{
		Status: v1.NodeStatus{
			Conditions: conds,
		},
	}
	got := getNodeConditions(n)
	if len(got) != 1 || got[0].Type != v1.NodeReady || got[0].Status != v1.ConditionTrue {
		t.Fatalf("getNodeConditions() = %#v, want %#v", got, conds)
	}
}

func TestIsNodeReady(t *testing.T) {
	n := &v1.Node{}
	if isNodeReady(n) {
		t.Fatalf("node with no conditions should not be ready")
	}

	n.Status.Conditions = []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}}
	if isNodeReady(n) {
		t.Fatalf("node with NodeReady=False should not be ready")
	}

	n.Status.Conditions[0].Status = v1.ConditionTrue
	if !isNodeReady(n) {
		t.Fatalf("node with NodeReady=True should be ready")
	}
}

func TestGetNodeTaints(t *testing.T) {
	ts := []v1.Taint{{Key: "foo"}, {Key: "bar"}}
	n := &v1.Node{Spec: v1.NodeSpec{Taints: ts}}
	got := getNodeTaints(n)
	if len(got) != 2 || got[0].Key != "foo" || got[1].Key != "bar" {
		t.Fatalf("getNodeTaints() = %#v, want %#v", got, ts)
	}
}

func TestIsNodeNoScheduleConditionTainted(t *testing.T) {
	n := &v1.Node{}
	if isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("empty node should not have NoScheduleCondTaint")
	}

	n.Spec.Taints = []v1.Taint{{
		Key:    "node.kubernetes.io/not-ready",
		Effect: v1.TaintEffectNoSchedule,
	}}
	if !isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("node with not-ready NoSchedule taint should return true")
	}

	n.Spec.Taints = []v1.Taint{{
		Key:    "node.kubernetes.io/unreachable",
		Effect: v1.TaintEffectNoSchedule,
	}}
	if !isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("node with unreachable NoSchedule taint should return true")
	}

	n.Spec.Taints = []v1.Taint{{
		Key:    "node.kubernetes.io/not-ready",
		Effect: v1.TaintEffectPreferNoSchedule,
	}}
	if isNodeNoScheduleConditionTainted(n) {
		t.Fatalf("PreferNoSchedule should not count as NoScheduleCondTaint")
	}
}

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

func TestIsNodeUnschedulable(t *testing.T) {
	n := &v1.Node{Spec: v1.NodeSpec{Unschedulable: false}}
	if isNodeUnschedulable(n) {
		t.Fatalf("node with Unschedulable=false should not be unschedulable")
	}
	n.Spec.Unschedulable = true
	if !isNodeUnschedulable(n) {
		t.Fatalf("node with Unschedulable=true should be unschedulable")
	}
}

func TestIsNodeUsable(t *testing.T) {
	if isNodeUsable(nil) {
		t.Fatalf("nil node should not be usable")
	}

	// control plane
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("control plane node should not be usable")
	}

	// unschedulable
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       v1.NodeSpec{Unschedulable: true},
	}
	if isNodeUsable(n) {
		t.Fatalf("unschedulable node should not be usable")
	}

	// not ready
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("not-ready node should not be usable")
	}

	// ready but tainted
	n = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n3"},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
		Spec: v1.NodeSpec{
			Taints: []v1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: v1.TaintEffectNoSchedule}},
		},
	}
	if isNodeUsable(n) {
		t.Fatalf("tainted node should not be usable")
	}

	// ready, no taints, zero allocatable
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

	// fully usable
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

//
// -----------------------------------------------------------------------------
// Pod resource helpers: getPodContainers / container requests / pod requests / priority
// -----------------------------------------------------------------------------

func TestGetPodContainers(t *testing.T) {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "c1"}, {Name: "c2"}},
		},
	}
	got := getPodContainers(p)
	if len(got) != 2 || got[0].Name != "c1" || got[1].Name != "c2" {
		t.Fatalf("getPodContainers() = %#v, want [c1,c2]", got)
	}
}

func TestGetContainerCPURequest(t *testing.T) {
	c := v1.Container{
		Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("250m"),
			},
		},
	}
	if got := getContainerCPURequest(c); got != 250 {
		t.Fatalf("getContainerCPURequest() = %d, want 250", got)
	}
}

func TestGetContainerMemoryRequest(t *testing.T) {
	q := resource.MustParse("64Mi")
	c := v1.Container{
		Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: q,
			},
		},
	}
	want := q.Value()
	if got := getContainerMemoryRequest(c); got != want {
		t.Fatalf("getContainerMemoryRequest() = %d, want %d", got, want)
	}
}

func TestGetPodCPURequest(t *testing.T) {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
				}},
				{Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m")},
				}},
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

func TestGetPodMemoryRequest(t *testing.T) {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("64Mi")},
				}},
				{Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("128Mi")},
				}},
			},
		},
	}
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

func TestGetPodAssignedNodeName(t *testing.T) {
	tests := []struct {
		name string
		pod  *v1.Pod
		want string
	}{
		{
			name: "assigned pod",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					NodeName: "node-1",
				},
			},
			want: "node-1",
		},
		{
			name: "unassigned pod",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					NodeName: "",
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPodAssignedNodeName(tt.pod)
			if got != tt.want {
				t.Fatalf("getPodAssignedNodeName() = %q, want %q", got, tt.want)
			}
		})
	}
}

//
// -----------------------------------------------------------------------------
// isPodDeleted / isPodAssigned / podsByUID
// -----------------------------------------------------------------------------

func TestIsPodDeleted(t *testing.T) {
	if !isPodDeleted(nil) {
		t.Fatalf("isPodDeleted(nil) = false, want true")
	}
	p := &v1.Pod{}
	if isPodDeleted(p) {
		t.Fatalf("isPodDeleted(p without DeletionTimestamp) = true, want false")
	}
	now := metav1.NewTime(time.Now())
	p.DeletionTimestamp = &now
	if !isPodDeleted(p) {
		t.Fatalf("isPodDeleted(p with DeletionTimestamp) = false, want true")
	}
}

func TestIsPodAssigned(t *testing.T) {
	if isPodAssigned(nil) {
		t.Fatalf("isPodAssigned(nil) = true, want false")
	}
	p := &v1.Pod{Spec: v1.PodSpec{NodeName: ""}}
	if isPodAssigned(p) {
		t.Fatalf("isPodAssigned(p with empty NodeName) = true, want false")
	}
	p.Spec.NodeName = "n1"
	if !isPodAssigned(p) {
		t.Fatalf("isPodAssigned(p with NodeName) = false, want true")
	}
}

func TestIsPodProtected(t *testing.T) {
	tests := []struct {
		name string
		pod  *v1.Pod
		want bool
	}{
		{
			name: "nil pod",
			pod:  nil,
			want: false,
		},
		{
			name: "kube-system namespace",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "controller",
					Namespace: "kube-system",
				},
			},
			want: true,
		},
		{
			name: "non kube-system namespace",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app",
					Namespace: "default",
				},
			},
			want: false,
		},
		{
			name: "empty namespace",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "no-namespace",
				},
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := isPodProtected(test.pod)
			if got != test.want {
				t.Fatalf("isPodProtected() = %v, want %v", got, test.want)
			}
		})
	}
}

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

//
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
	// pending pod should be ignored
	pPending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns", UID: types.UID("u2")},
		Spec:       v1.PodSpec{NodeName: ""},
	}

	// Order 1 vs order 2 → determinism
	fp1 := clusterFingerprint([]*v1.Node{n2, n1}, []*v1.Pod{pPending, p1})
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
	pPending2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "ns", UID: types.UID("u3")},
		Spec:       v1.PodSpec{NodeName: ""},
	}
	fp4 := clusterFingerprint([]*v1.Node{n1, n2}, []*v1.Pod{p1, pPending, pPending2})
	if fp1 != fp4 {
		t.Fatalf("clusterFingerprint should ignore changes in pending pods")
	}

	// Extra coverage:
	// - nil node
	// - unusable node (control-plane)
	// - pod scheduled on unusable node (must be ignored by usableNames check).
	nBad := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "bad",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
	}
	pOnBad := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-bad", Namespace: "ns", UID: types.UID("ubad")},
		Spec: v1.PodSpec{
			NodeName: "bad",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("50m"),
						v1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}

	fp5 := clusterFingerprint([]*v1.Node{nil, nBad, n1, n2}, []*v1.Pod{p1, pOnBad, pPending})
	if fp5 != fp1 {
		t.Fatalf("clusterFingerprint should ignore nil/unusable nodes and pods on them; fp5=%q base=%q", fp5, fp1)
	}
}

// This test exists purely to exercise the sort.Slice comparator branches in the
// pod-key sorting logic inside clusterFingerprint:
//   - keys[i].node != keys[j].node  (different nodes)
//   - keys[i].node == keys[j].node  (same node, compare UID)
func TestClusterFingerprint_SortingBranches(t *testing.T) {
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
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	makePod := func(name string, uid types.UID, node string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid},
			Spec: v1.PodSpec{
				NodeName: node,
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
	}

	// Two pods on n1 (same node, different UIDs) + one on n2 (different node).
	p1 := makePod("p1", types.UID("u1"), "n1")
	p2 := makePod("p2", types.UID("u2"), "n1")
	p3 := makePod("p3", types.UID("u3"), "n2")

	fpA := clusterFingerprint([]*v1.Node{n2, n1}, []*v1.Pod{p3, p2, p1})
	fpB := clusterFingerprint([]*v1.Node{n1, n2}, []*v1.Pod{p1, p3, p2})
	if fpA != fpB {
		t.Fatalf("clusterFingerprint not deterministic with multiple pods across nodes; fpA=%q fpB=%q", fpA, fpB)
	}
}

//
// -----------------------------------------------------------------------------
// isPodPreemptor
// -----------------------------------------------------------------------------

func TestIsPodPreemptor(t *testing.T) {
	u1 := types.UID("u1")
	u2 := types.UID("u2")

	if !isPodPreemptor(u1, u1) {
		t.Fatalf("expected isPodPreemptor(u1,u1) to be true")
	}
	if isPodPreemptor(u1, u2) {
		t.Fatalf("expected isPodPreemptor(u1,u2) to be false")
	}
	if isPodPreemptor("", u2) {
		t.Fatalf("expected isPodPreemptor('',u2) to be false")
	}
	if isPodPreemptor(u1, "") {
		t.Fatalf("expected isPodPreemptor(u1,'') to be false")
	}
}

//
// -----------------------------------------------------------------------------
// WorkloadKey.String
// -----------------------------------------------------------------------------

func TestWorkloadKeyString(t *testing.T) {
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

//
// -----------------------------------------------------------------------------
// topWorkload
// -----------------------------------------------------------------------------

func TestTopWorkload(t *testing.T) {
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

	p = p.DeepCopy()
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "ss1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkStatefulSet || wk.Name != "ss1" {
		t.Fatalf("topWorkload(SS) = %#v (ok=%v), want ss:ns/ss1", wk, ok)
	}

	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkDaemonSet || wk.Name != "ds1" {
		t.Fatalf("topWorkload(DS) = %#v (ok=%v), want ds:ns/ds1", wk, ok)
	}

	p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "job1", Controller: &ctrlTrue}}
	wk, ok = topWorkload(p)
	if !ok || wk.Kind != wkJob || wk.Name != "job1" {
		t.Fatalf("topWorkload(Job) = %#v (ok=%v), want job:ns/job1", wk, ok)
	}

	p.OwnerReferences = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "rs1", Controller: nil},
	}
	wk, ok = topWorkload(p)
	if ok {
		t.Fatalf("expected topWorkload to return false when no Controller=true owner exists, got %#v", wk)
	}
}
