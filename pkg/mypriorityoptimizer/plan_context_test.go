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
// --------------------------

func TestPlanContext_NodeListError(t *testing.T) {
	pl := &SharedState{}

	nl := &fakeNodeLister{
		nodes: nil,
		err:   errors.New("nodes failed"),
	}

	withNodeLister(nl, func() {
		nodes, pods, pending, _, err := pl.planContext(nil)

		if err != ErrFailedToListNodes {
			t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToListNodes)
		}
		if nodes != nil {
			t.Fatalf("nodes = %+v, want nil on node list error", nodes)
		}
		if pods != nil {
			t.Fatalf("pods = %+v, want nil on node list error", pods)
		}
		if pending != 0 {
			t.Fatalf("pendingPrePlan = %d, want 0 on node list error", pending)
		}
	})
}

func TestPlanContext_PodListError(t *testing.T) {
	pl := &SharedState{}

	nodes := []*v1.Node{
		newNode("n1"),
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
			gotNodes, pods, pending, _, err := pl.planContext(nil)

			if err != ErrFailedToListPods {
				t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToListPods)
			}
			if len(gotNodes) != 1 || gotNodes[0].Name != "n1" {
				t.Fatalf("nodes = %+v, want single node n1", gotNodes)
			}
			if pods != nil {
				t.Fatalf("pods = %+v, want nil on pod list error", pods)
			}
			if pending != 0 {
				t.Fatalf("pendingPrePlan = %d, want 0 on pod list error", pending)
			}
		})
	})
}

func TestPlanContext_NoPendingPods(t *testing.T) {
	pl := &SharedState{}

	nodes := []*v1.Node{
		newNode("n1"),
	}

	// All pods are already assigned -> no pending pods.
	running1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns",
			UID:       "u1",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
	running2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p2",
			Namespace: "ns",
			UID:       "u2",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}

	store := map[string]map[string]*v1.Pod{
		"ns": {
			"p1": running1,
			"p2": running2,
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
			gotNodes, gotPods, pending, _, err := pl.planContext(nil)

			if err != ErrNoPendingPods {
				t.Fatalf("planContext() error = %v, want %v", err, ErrNoPendingPods)
			}
			if len(gotNodes) != 1 || gotNodes[0].Name != "n1" {
				t.Fatalf("nodes = %+v, want single node n1", gotNodes)
			}
			if len(gotPods) != 2 {
				t.Fatalf("pods len = %d, want 2", len(gotPods))
			}
			if pending != 0 {
				t.Fatalf("pendingPrePlan = %d, want 0 when all pods are running", pending)
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
			gotNodes, gotPods, pendingCount, _, err := pl.planContext(nil)

			if err != ErrFailedToBuildSolverInput {
				t.Fatalf("planContext() error = %v, want %v", err, ErrFailedToBuildSolverInput)
			}
			if len(gotNodes) != 0 {
				t.Fatalf("nodes len = %d, want 0", len(gotNodes))
			}
			if len(gotPods) != 1 {
				t.Fatalf("pods len = %d, want 1", len(gotPods))
			}
			if pendingCount != 1 {
				t.Fatalf("pendingPrePlan = %d, want 1 (one pending pod)", pendingCount)
			}
		})
	})
}

// -------------------------
// planContext – happy path
// --------------------------

func TestPlanContext_Success(t *testing.T) {
	pl := &SharedState{}

	nodes := []*v1.Node{
		newNode("n1"),
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
			gotNodes, gotPods, pendingCount, inp, err := pl.planContext(nil)
			if err != nil {
				t.Fatalf("planContext() unexpected error: %v", err)
			}

			if len(gotNodes) != 1 || gotNodes[0].Name != "n1" {
				t.Fatalf("nodes = %+v, want single node n1", gotNodes)
			}
			if len(gotPods) != 2 {
				t.Fatalf("pods len = %d, want 2", len(gotPods))
			}
			if pendingCount != 1 {
				t.Fatalf("pendingPrePlan = %d, want 1 (one pending pod)", pendingCount)
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
