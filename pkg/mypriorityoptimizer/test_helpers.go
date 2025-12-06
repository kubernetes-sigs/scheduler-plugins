// test_helpers.go
package mypriorityoptimizer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// withMode is a small helper to temporarily set the mode during a test
// and restore to the original values.
func withMode(mode ModeType, stage StageType, synch bool, fn func()) {
	oldMode := OptimizeMode
	oldStage := OptimizeHookStage
	oldSynch := OptimizeSolveSynch

	OptimizeMode = mode
	OptimizeHookStage = stage
	OptimizeSolveSynch = synch
	defer func() {
		OptimizeMode = oldMode
		OptimizeHookStage = oldStage
		OptimizeSolveSynch = oldSynch
	}()
	fn()
}

// withAppendStatsHook temporarily overrides appendSolverStatsCMHook and restores
// it after fn returns.
func withAppendStatsHook(
	hook func(pl *SharedState, ctx context.Context, entry ExportedSolverStats),
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

func uidSet(uids ...string) map[types.UID]struct{} {
	m := make(map[types.UID]struct{}, len(uids))
	for _, u := range uids {
		m[types.UID(u)] = struct{}{}
	}
	return m
}
