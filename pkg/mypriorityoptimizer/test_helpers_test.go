// test_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// withMode is a small helper to temporarily set the mode during a test
// and restore to the original values.
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

// withAppendStatsHook temporarily overrides appendSolverStatsCMHook and restores
// it after fn returns.
func withAppendStatsHook(
	hook func(pl *SharedState, ctx context.Context, entry ExportedPlannerStats),
	fn func(),
) {
	orig := appendSolverStatsCMHook
	appendSolverStatsCMHook = hook
	defer func() { appendSolverStatsCMHook = orig }()
	fn()
}

func writeFakeSolverScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake_solver.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("failed to write fake solver script: %v", err)
	}
	return path
}

// newPod creates a pod with the given ns/name/uid, optional nodeName, and priority.
func newPod(ns, name, uid, node string, prio int32) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			UID:       types.UID(uid),
		},
		Spec: v1.PodSpec{
			NodeName: node,
			Priority: &prio,
		},
	}
}

// newNode creates a schedulable, Ready node
func newNode(name string) *v1.Node {
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

func uidSet(uids ...string) map[types.UID]struct{} {
	m := make(map[types.UID]struct{}, len(uids))
	for _, u := range uids {
		m[types.UID(u)] = struct{}{}
	}
	return m
}

// -----------------------------------------------------------------------------
// Plugin readiness helpers (override global vars for the duration of a test)
// -----------------------------------------------------------------------------

func withReadinessInterval(d time.Duration, fn func()) {
	old := readinessUsableNodeInterval
	readinessUsableNodeInterval = d
	defer func() { readinessUsableNodeInterval = old }()
	fn()
}

func withReadinessHooks(
	getNodes func(*SharedState) ([]*v1.Node, error),
	isUsable func(*v1.Node) bool,
	fn func(),
) {
	oldGet := getNodesForReadiness
	oldUsable := isNodeUsableForReadiness

	getNodesForReadiness = getNodes
	isNodeUsableForReadiness = isUsable

	defer func() {
		getNodesForReadiness = oldGet
		isNodeUsableForReadiness = oldUsable
	}()

	fn()
}
