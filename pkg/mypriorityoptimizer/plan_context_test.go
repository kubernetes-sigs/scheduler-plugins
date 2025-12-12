// plan_context_test.go
package mypriorityoptimizer

import (
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -------------------------
// planContext – error paths
// -------------------------

func TestPlanContext_NodeListError(t *testing.T) {
	pl := &SharedState{}

	nl := &fakeNodeLister{
		nodes: nil,
		err:   errors.New("nodes failed"),
	}

	withNodeLister(nl, func() {
		nodes, pods, _, err := pl.planContext(nil)

		if err != ErrFailedToListNodes {
			t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToListNodes)
		}
		if nodes != nil {
			t.Fatalf("nodes = %+v, want nil on node list error", nodes)
		}
		if pods != nil {
			t.Fatalf("pods = %+v, want nil on node list error", pods)
		}
	})
}

func TestPlanContext_PodListError(t *testing.T) {
	pl := &SharedState{}

	nodes := []*v1.Node{
		makeNode("n1"),
	}

	nl := &fakeNodeLister{
		nodes: nodes,
		err:   nil,
	}

	plister := &fakePodLister{
		store: nil,
		err:   errors.New("pods failed"),
	}

	withNodeLister(nl, func() {
		withPodLister(plister, func() {
			gotNodes, pods, _, err := pl.planContext(nil)

			if err != ErrFailedToListPods {
				t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToListPods)
			}
			if len(gotNodes) != 1 || gotNodes[0].Name != "n1" {
				t.Fatalf("nodes = %+v, want single node n1", gotNodes)
			}
			if pods != nil {
				t.Fatalf("pods = %+v, want nil on pod list error", pods)
			}
		})
	})
}

func TestPlanContext_BuildSolverInputError(t *testing.T) {
	pl := &SharedState{}

	// No usable nodes (empty slice) so buildSolverInput will fail with ErrNoUsableNodes
	// and planContext should map that to ErrFailedToBuildSolverInput.
	nodes := []*v1.Node{}

	// One pending pod so we get past the "no pending pods" check.
	pending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-pending",
			Namespace: "ns",
			UID:       "u-pending",
		},
		Spec: v1.PodSpec{
			NodeName: "",
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
		},
	}

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-pending": pending,
		},
	}

	nl := &fakeNodeLister{
		nodes: nodes,
		err:   nil,
	}
	plister := &fakePodLister{
		store: store,
		err:   nil,
	}

	withNodeLister(nl, func() {
		withPodLister(plister, func() {
			gotNodes, gotPods, _, err := pl.planContext(nil)

			if err != ErrFailedToBuildSolverInput {
				t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToBuildSolverInput)
			}
			if len(gotNodes) != 0 {
				t.Fatalf("nodes len = %d, want 0", len(gotNodes))
			}
			if len(gotPods) != 1 {
				t.Fatalf("pods len = %d, want 1", len(gotPods))
			}
		})
	})
}

// -------------------------
// planContext – happy path
// -------------------------

func TestPlanContext_Success(t *testing.T) {
	pl := &SharedState{}

	nodes := []*v1.Node{
		makeNode("n1"),
	}

	// One running pod + one pending pod.
	running := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-run",
			Namespace: "ns",
			UID:       "u-run",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
	pending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-pending",
			Namespace: "ns",
			UID:       "u-pending",
		},
		Spec: v1.PodSpec{
			NodeName: "",
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
		},
	}

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p-run":     running,
			"p-pending": pending,
		},
	}

	nl := &fakeNodeLister{
		nodes: nodes,
		err:   nil,
	}
	plister := &fakePodLister{
		store: store,
		err:   nil,
	}

	withNodeLister(nl, func() {
		withPodLister(plister, func() {
			gotNodes, gotPods, inp, err := pl.planContext(nil)
			if err != nil {
				t.Fatalf("planContext() unexpected error: %v", err)
			}

			if len(gotNodes) != 1 || gotNodes[0].Name != "n1" {
				t.Fatalf("nodes = %+v, want single node n1", gotNodes)
			}
			if len(gotPods) != 2 {
				t.Fatalf("pods len = %d, want 2", len(gotPods))
			}

			// Baseline score in the SolverInput should match what buildBaselineScore
			// would compute for the same pods (i.e., only the running pod counts).
			wantBaseline := buildBaselineScore(gotPods)

			if inp.BaselineScore.Evicted != wantBaseline.Evicted ||
				inp.BaselineScore.Moved != wantBaseline.Moved {
				t.Fatalf("Baseline mismatch: got %+v, want %+v", inp.BaselineScore, wantBaseline)
			}

			if len(inp.BaselineScore.PlacedByPriority) != len(wantBaseline.PlacedByPriority) {
				t.Fatalf("Baseline.PlacedByPriority len = %d, want %d",
					len(inp.BaselineScore.PlacedByPriority),
					len(wantBaseline.PlacedByPriority),
				)
			}
			for k, v := range wantBaseline.PlacedByPriority {
				if inp.BaselineScore.PlacedByPriority[k] != v {
					t.Fatalf("Baseline.PlacedByPriority[%q] = %d, want %d",
						k, inp.BaselineScore.PlacedByPriority[k], v)
				}
			}
		})
	})
}
