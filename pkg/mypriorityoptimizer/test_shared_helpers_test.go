// test_shared_helpers_test.go
package mypriorityoptimizer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fwk "k8s.io/kube-scheduler/framework"
)

// -------------------------
// withMode
// -------------------------

// withMode is a small helper to temporarily set the mode during a test and
// restore to the original values.
func withMode(mode ModeType, synch bool, fn func()) {
	oldMode := OptimizeMode
	oldSynch := OptimizeSolveSynch

	OptimizeMode = mode
	OptimizeSolveSynch = synch
	defer func() {
		OptimizeMode = oldMode
		OptimizeSolveSynch = oldSynch
	}()
	fn()
}

// -------------------------
// writeFakeSolverScript
// -------------------------

// writeFakeSolverScript writes a fake solver script to the specified directory
// with the specified body, and returns the full path to the script.
func writeFakeSolverScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake_solver.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("failed to write fake solver script: %v", err)
	}
	return path
}

// -------------------------
// makePod
// -------------------------

// makePod creates a pod with the specified attributes.
func makePod(ns, name, uid, node, ownerKind, ownerName string, prio int32) *v1.Pod {
	var ownerRefs []metav1.OwnerReference
	if ownerKind != "" && ownerName != "" {
		controller := true
		ownerRefs = []metav1.OwnerReference{
			{
				APIVersion: "apps/v1",
				Kind:       ownerKind,
				Name:       ownerName,
				Controller: &controller,
			},
		}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			UID:             types.UID(uid),
			OwnerReferences: ownerRefs,
		},
		Spec: v1.PodSpec{
			NodeName: node,
			Priority: &prio,
		},
	}
}

// -------------------------
// makeNode
// -------------------------

// makeNode creates a node with the specified name.
func makeNode(name string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{Type: v1.NodeReady, Status: v1.ConditionTrue},
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
}

// -------------------------
// mustStatus
// -------------------------

// mustHookStatus asserts the framework status code and (optionally) that the message contains a substring.
func mustHookStatus(t *testing.T, stage string, st *fwk.Status, want fwk.Code, contains string) {
	t.Helper()
	if st == nil {
		t.Fatalf("%s() returned nil status", stage)
	}
	if st.Code() != want {
		t.Fatalf("%s() code = %v, want %v (msg=%q)", stage, st.Code(), want, st.Message())
	}
	if contains != "" && !strings.Contains(st.Message(), contains) {
		t.Fatalf("%s() message = %q, want to contain %q", stage, st.Message(), contains)
	}
}
