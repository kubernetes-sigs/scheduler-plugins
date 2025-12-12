// solver_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// -------------------------
// Helpers
// --------------------------

func hasKey(args []any, key string) bool {
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok && k == key {
			return true
		}
	}
	return false
}

// withAppendStatsHook temporarily overrides appendSolverStatsCMHook and
// restores it after fn returns.
func withAppendStatsHook(
	hook func(pl *SharedState, ctx context.Context, entry ExportedSolverStats),
	fn func(),
) {
	orig := appendSolverStatsCMHook
	appendSolverStatsCMHook = hook
	defer func() { appendSolverStatsCMHook = orig }()
	fn()
}

// -------------------------
// isAnySolverEnabled
// --------------------------

func TestIsAnySolverEnabled(t *testing.T) {
	origPy := SolverPythonEnabled
	defer func() {
		SolverPythonEnabled = origPy
	}()

	SolverPythonEnabled = false
	pl := &SharedState{}

	if got := pl.isAnySolverEnabled(); got {
		t.Fatalf("isAnySolverEnabled() with all solvers disabled = %v, want false", got)
	}

	SolverPythonEnabled = true
	if got := pl.isAnySolverEnabled(); !got {
		t.Fatalf("isAnySolverEnabled() with python enabled = %v, want true", got)
	}
}

// -------------------------
// buildSolverInput
// --------------------------

func TestBuildSolverInput_NoUsableNodes(t *testing.T) {
	pl := &SharedState{}

	in, err := pl.buildSolverInput(nil, nil, nil)
	if err == nil {
		t.Fatalf("buildSolverInput() error = nil, want ErrNoUsableNodes")
	}
	if !errors.Is(err, ErrNoUsableNodes) {
		t.Fatalf("buildSolverInput() error = %v, want ErrNoUsableNodes", err)
	}
	if len(in.Nodes) != 0 || len(in.Pods) != 0 {
		t.Fatalf("buildSolverInput() with no nodes returned non-empty input: %+v", in)
	}
}

func TestBuildSolverInput_WithNodesPodsAndPreemptor(t *testing.T) {
	pl := &SharedState{}

	// One usable node.
	nUsable := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
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
	// One unusable node (unschedulable) – should be ignored.
	nUnusable := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		Spec:       v1.NodeSpec{Unschedulable: true},
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

	// Pending pod -> always included.
	pPending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-pending",
			Namespace: "ns",
			UID:       "u-pending",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}

	// Running on usable node -> included.
	pRunUsable := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-run-usable",
			Namespace: "ns",
			UID:       "u-run-usable",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("200m"),
						v1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}

	// Running on unusable node -> ignored.
	pRunUnusable := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-run-unusable",
			Namespace: "ns",
			UID:       "u-run-unusable",
		},
		Spec: v1.PodSpec{
			NodeName: "n2",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("300m"),
						v1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}

	// System namespace pending pod -> should be Protected=true.
	pSystem := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-sys",
			Namespace: SystemNamespace,
			UID:       "u-sys",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("50m"),
						v1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
		},
	}

	// Preemptor pod – present in the live pods slice but should only appear in Preemptor.
	preemptor := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-preemptor",
			Namespace: "ns",
			UID:       "u-preemptor",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}

	// Duplicate pending pod to exercise deduplication.
	pods := []*v1.Pod{
		pPending,
		pRunUsable,
		pRunUnusable,
		preemptor,
		pSystem,
		pPending, // duplicate UID
		nil,      // ignored
	}

	in, err := pl.buildSolverInput([]*v1.Node{nUsable, nUnusable}, pods, preemptor)
	if err != nil {
		t.Fatalf("buildSolverInput() unexpected error: %v", err)
	}

	if len(in.Nodes) != 1 || in.Nodes[0].Name != "n1" {
		t.Fatalf("buildSolverInput().Nodes = %+v, want single usable node n1", in.Nodes)
	}
	if in.Preemptor == nil || string(in.Preemptor.UID) != "u-preemptor" {
		t.Fatalf("Preemptor = %#v, want UID u-preemptor", in.Preemptor)
	}

	// Expect: pending, running-on-usable, system pending (protected)
	if len(in.Pods) != 3 {
		t.Fatalf("buildSolverInput().Pods len = %d, want 3", len(in.Pods))
	}

	var seenPending, seenRunUsable, seenSystem bool
	for _, sp := range in.Pods {
		switch string(sp.UID) {
		case "u-pending":
			if sp.Node != "" {
				t.Fatalf("pending pod Node = %q, want empty", sp.Node)
			}
			seenPending = true
		case "u-run-usable":
			if sp.Node != "n1" {
				t.Fatalf("running pod Node = %q, want n1", sp.Node)
			}
			seenRunUsable = true
		case "u-sys":
			if !sp.Protected {
				t.Fatalf("system pod must be Protected=true")
			}
			seenSystem = true
		default:
			t.Fatalf("unexpected Pod UID %q in input", sp.UID)
		}
	}

	if !seenPending || !seenRunUsable || !seenSystem {
		t.Fatalf("missing expected pods: pending=%v runUsable=%v system=%v",
			seenPending, seenRunUsable, seenSystem)
	}
}

// -------------------------
// buildBaselineScore
// --------------------------

func TestBuildBaselineScore(t *testing.T) {
	p1Pri := int32(1)
	p2Pri := int32(2)

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "p1",
				Namespace: "ns",
				Name:      "pod1",
			},
			Spec: v1.PodSpec{
				NodeName: "n1",   // assigned -> counted as placed
				Priority: &p1Pri, // priority 1
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "p2",
				Namespace: "ns",
				Name:      "pod2",
			},
			Spec: v1.PodSpec{
				NodeName: "n2",   // assigned -> counted as placed
				Priority: &p2Pri, // priority 2
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "p3",
				Namespace: "ns",
				Name:      "pod3",
			},
			Spec: v1.PodSpec{
				// no NodeName -> pending, should NOT count as placed
			},
			Status: v1.PodStatus{
				Phase: v1.PodPending,
			},
		},
	}

	score := buildBaselineScore(pods)

	if score.Evicted != 0 || score.Moved != 0 {
		t.Fatalf("baseline score Evicted/Moved = (%d,%d), want (0,0)", score.Evicted, score.Moved)
	}

	if got := score.PlacedByPriority["1"]; got != 1 {
		t.Fatalf("PlacedByPriority['1'] = %d, want 1", got)
	}
	if got := score.PlacedByPriority["2"]; got != 1 {
		t.Fatalf("PlacedByPriority['2'] = %d, want 1", got)
	}

	// Optional extra sanity check: no unexpected priorities
	if len(score.PlacedByPriority) != 2 {
		t.Fatalf("len(PlacedByPriority) = %d, want 2", len(score.PlacedByPriority))
	}
}

// -------------------------
// solverConfigArgs
// --------------------------

func TestSolverConfigArgs(t *testing.T) {
	origPy := SolverPythonEnabled
	origSave := SolverSaveAllAttempts
	defer func() {
		SolverPythonEnabled = origPy
		SolverSaveAllAttempts = origSave
	}()

	// Case 1: all solvers disabled
	SolverPythonEnabled = false
	SolverSaveAllAttempts = false

	args := solverConfigArgs()
	if hasKey(args, "pythonSolver") {
		t.Fatalf("solverConfigArgs() should not contain solver keys when all disabled, got %v", args)
	}
	if !hasKey(args, "saveFailedAttempts") {
		t.Fatalf("solverConfigArgs() must always include shared flags, got %v", args)
	}

	// Case 2: python only
	SolverPythonEnabled = true

	args = solverConfigArgs()
	if !hasKey(args, "pythonSolver") {
		t.Fatalf("solverConfigArgs() missing pythonSolver when python enabled, got %v", args)
	}
}

// -------------------------
// isSolutionBetter
// --------------------------

func TestIsSolutionBetter_Order(t *testing.T) {
	base := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          2,
		Moved:            3,
	}

	// Better placed high-prio
	suggBetterPlaced := SolverScore{
		PlacedByPriority: map[string]int{"1": 2, "0": 0},
		Evicted:          2,
		Moved:            3,
	}
	if got := isSolutionBetter(&base, &suggBetterPlaced); got != 1 {
		t.Fatalf("isSolutionBetter() placed better = %d, want 1", got)
	}

	// Same placed, fewer evictions
	suggBetterEvict := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          1,
		Moved:            3,
	}
	if got := isSolutionBetter(&base, &suggBetterEvict); got != 1 {
		t.Fatalf("isSolutionBetter() fewer evictions = %d, want 1", got)
	}

	// Same placed/evictions, more moves (worse)
	suggMoreMoves := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          2,
		Moved:            4,
	}
	if got := isSolutionBetter(&base, &suggMoreMoves); got != -1 {
		t.Fatalf("isSolutionBetter() more moves = %d, want -1", got)
	}

	// Exactly equal
	same := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          2,
		Moved:            3,
	}
	if got := isSolutionBetter(&base, &same); got != 0 {
		t.Fatalf("isSolutionBetter() equal = %d, want 0", got)
	}
}

// -------------------------
// isSolutionUsable
// --------------------------

func TestIsSolutionUsable(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"", false},
		{"OPTIMAL", true},
		{"FEASIBLE", true},
		{"INFEASIBLE", false},
	}

	for _, tt := range tests {
		if got := isSolutionUsable(tt.status); got != tt.want {
			t.Fatalf("isSolutionUsable(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

// -------------------------
// isSolutionApplicable
// --------------------------

func TestSolutionApplicable_NilPlan(t *testing.T) {
	pl := &SharedState{}
	ok, reason := pl.isSolutionApplicable(nil, nil, nil)
	if ok {
		t.Fatalf("isSolutionApplicable(nil, ...) = true, want false")
	}
	if reason != "nil plan" {
		t.Fatalf("isSolutionApplicable(nil, ...) reason = %q, want %q", reason, "nil plan")
	}
}

func TestSolutionApplicable_CapacityExceededWhenNoNodes(t *testing.T) {
	pl := &SharedState{}

	// One running pod on some node, but we pass *no* nodes to isSolutionApplicable.
	// That makes capacity map zero for that node, but usage > 0 ⇒ capacity exceeded.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
			UID:       "uid-1",
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("100m"),
							v1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}

	out := &SolverOutput{} // empty plan; we only care about capacity check

	ok, reason := pl.isSolutionApplicable(out, nil, []*v1.Pod{pod})
	if ok {
		t.Fatalf("isSolutionApplicable() with used resources but no node capacities = true, want false")
	}
	if !strings.Contains(reason, "capacity exceeded") {
		t.Fatalf("isSolutionApplicable() reason = %q, want it to contain 'capacity exceeded'", reason)
	}
}

func TestSolutionApplicable_EvictNodeNowUnusable(t *testing.T) {
	pl := &SharedState{}

	// Running pod on node1, but we pass no usable nodes -> eviction sees node unusable.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
			UID:       "uid-1",
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
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

	out := &SolverOutput{
		Evictions: []SolverPod{
			{UID: "uid-1", Namespace: "ns1", Name: "p1"},
		},
	}

	ok, reason := pl.isSolutionApplicable(out, nil, []*v1.Pod{pod})
	if ok {
		t.Fatalf("isSolutionApplicable() with eviction on unusable node = true, want false")
	}
	if !strings.Contains(reason, "evict node now unusable") {
		t.Fatalf("isSolutionApplicable() reason = %q, want it to contain 'evict node now unusable'", reason)
	}
}

func TestSolutionApplicable_PendingPreconditionChanged(t *testing.T) {
	pl := &SharedState{}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
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

	// Pod is already bound (no longer pending) but plan expects FromNode == "".
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
			UID:       "uid-1",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
		},
	}

	out := &SolverOutput{
		Placements: []SolverPod{
			{
				UID:       "uid-1",
				Namespace: "ns1",
				Name:      "p1",
				OldNode:   "",
				Node:      "n1",
			},
		},
	}

	ok, reason := pl.isSolutionApplicable(out, []*v1.Node{node}, []*v1.Pod{pod})
	if ok {
		t.Fatalf("isSolutionApplicable() with pending precondition violated = true, want false")
	}
	if !strings.Contains(reason, "pending precondition changed") {
		t.Fatalf("isSolutionApplicable() reason = %q, want it to contain 'pending precondition changed'", reason)
	}
}

func TestSolutionApplicable_Success(t *testing.T) {
	pl := &SharedState{}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
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

	// pStay remains on n1.
	pStay := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-stay",
			Namespace: "ns",
			UID:       "u-stay",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("500m"),
						v1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}

	// pEvict will be evicted from n1.
	pEvict := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-evict",
			Namespace: "ns",
			UID:       "u-evict",
		},
		Spec: v1.PodSpec{
			NodeName: "n1",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("500m"),
						v1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}

	// pPending gets placed on n1.
	pPending := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-pending",
			Namespace: "ns",
			UID:       "u-pending",
		},
		Spec: v1.PodSpec{
			NodeName: "",
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("500m"),
						v1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}

	out := &SolverOutput{
		Evictions: []SolverPod{
			{UID: "u-evict", Namespace: "ns", Name: "p-evict"},
		},
		Placements: []SolverPod{
			{
				UID:       "u-pending",
				Namespace: "ns",
				Name:      "p-pending",
				OldNode:   "",
				Node:      "n1",
			},
		},
	}

	ok, reason := pl.isSolutionApplicable(out, []*v1.Node{node}, []*v1.Pod{pStay, pEvict, pPending})
	if !ok {
		t.Fatalf("isSolutionApplicable() = false, reason=%q, want true", reason)
	}
}

// -------------------------
// logLeaderboard
// --------------------------

func TestLogLeaderboard_DoesNotPanic(t *testing.T) {
	baseline := SolverScore{
		PlacedByPriority: map[string]int{"1": 1},
		Evicted:          0,
		Moved:            0,
	}

	attempts := []SolverResult{
		{
			Name:       "python",
			Status:     "OPTIMAL",
			DurationMs: 10,
			Score: SolverScore{
				PlacedByPriority: map[string]int{"1": 2}, // better
				Evicted:          0,
				Moved:            0,
			},
		},
		{
			Name:       "fallback",
			Status:     "FEASIBLE",
			DurationMs: 20,
			Score:      baseline, // equal to baseline
		},
	}

	best := attempts[0]

	// We just want to exercise the grouping and tie-tagging logic; if this
	// panics, the test fails.
	logLeaderboard("test-label", attempts, baseline, &best)
}

// -------------------------
// scoreSolution
// --------------------------

func TestScoreSolution_NilOutput(t *testing.T) {
	in := SolverInput{
		Pods: []SolverPod{
			{UID: "u1", Priority: 1, Node: "n1"},
		},
	}
	score := scoreSolution(in, nil)
	if len(score.PlacedByPriority) != 0 || score.Evicted != 0 || score.Moved != 0 {
		t.Fatalf("scoreSolution() with nil out = %#v, want zero score", score)
	}
}

func TestScoreSolution_Basic(t *testing.T) {
	in := SolverInput{
		Pods: []SolverPod{
			{UID: "u1", Priority: 1, Node: "n1"}, // running
			{UID: "u2", Priority: 2, Node: ""},   // pending
			{UID: "u3", Priority: 1, Node: "n1"}, // running
		},
	}

	out := &SolverOutput{
		Placements: []SolverPod{
			{UID: "u2", Node: "n1"}, // place pending
			{UID: "u3", Node: "n2"}, // move running
			{UID: "uX", Node: "n1"}, // unknown UID -> ignored
		},
		Evictions: []SolverPod{
			{UID: "u1"}, // evict u1
		},
	}

	score := scoreSolution(in, out)

	if got := score.PlacedByPriority["1"]; got != 1 {
		t.Fatalf("placed prio 1 = %d, want 1", got)
	}
	if got := score.PlacedByPriority["2"]; got != 1 {
		t.Fatalf("placed prio 2 = %d, want 1", got)
	}
	if score.Evicted != 1 {
		t.Fatalf("Evicted = %d, want 1", score.Evicted)
	}
	if score.Moved != 1 {
		t.Fatalf("Moved = %d, want 1", score.Moved)
	}
}

func TestScoreSolution_WithPreemptor(t *testing.T) {
	pre := &SolverPod{UID: "u-pre", Priority: 5}

	in := SolverInput{
		Preemptor: pre,
		Pods:      nil,
	}

	out := &SolverOutput{
		Placements: []SolverPod{
			{UID: pre.UID, Node: "n1"},
		},
	}

	score := scoreSolution(in, out)

	if got := score.PlacedByPriority["5"]; got != 1 {
		t.Fatalf("placed prio 5 (preemptor) = %d, want 1", got)
	}
	if score.Evicted != 0 || score.Moved != 0 {
		t.Fatalf("Evicted/Moved = (%d,%d), want (0,0)", score.Evicted, score.Moved)
	}
}

// -------------------------
// toSolverPod
// --------------------------

func TestToSolverPod_BasicMapping(t *testing.T) {
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mypod",
			Namespace: "ns",
			UID:       "uid-1",
		},
		Spec: v1.PodSpec{},
	}

	sp := toSolverPod(p, "nodeX")

	if sp.UID != p.UID || sp.Namespace != p.Namespace || sp.Name != p.Name {
		t.Fatalf("toSolverPod() identity fields mismatch: %+v", sp)
	}
	if sp.Node != "nodeX" {
		t.Fatalf("toSolverPod() Node = %q, want %q", sp.Node, "nodeX")
	}
	// With no resource requests / priority set, we at least expect 0 values.
	if sp.ReqCPUm != 0 || sp.ReqMemBytes != 0 || sp.Priority != 0 {
		t.Fatalf("toSolverPod() expected zero cpu/mem/priority, got cpu=%d mem=%d prio=%d",
			sp.ReqCPUm, sp.ReqMemBytes, sp.Priority)
	}
}

// -------------------------
// exportSolverStatsToConfigMap
// --------------------------

func TestExportSolverStatsToConfigMap_UsesAppendHook(t *testing.T) {
	pl := &SharedState{}

	baseline := SolverScore{
		PlacedByPriority: map[string]int{"1": 1},
		Evicted:          0,
		Moved:            0,
	}
	attempts := []SolverResult{
		{
			Name:       "python",
			Status:     "OPTIMAL",
			DurationMs: 42,
			Score: SolverScore{
				PlacedByPriority: map[string]int{"1": 2},
				Evicted:          0,
				Moved:            0,
			},
		},
	}

	var gotPl *SharedState
	var gotEntry ExportedSolverStats

	withAppendStatsHook(
		func(hpl *SharedState, _ context.Context, entry ExportedSolverStats) {
			gotPl = hpl
			gotEntry = entry
		},
		func() {
			pl.exportSolverStatsToConfigMap(
				context.Background(),
				"strategyX",
				baseline,
				"python",
				attempts,
				"some-error",
			)
		},
	)

	if gotPl != pl {
		t.Fatalf("hook received pl=%p, want %p", gotPl, pl)
	}
	if gotEntry.BestName != "python" {
		t.Fatalf("BestName = %q, want %q", gotEntry.BestName, "python")
	}
	if gotEntry.Error != "some-error" {
		t.Fatalf("Error = %q, want %q", gotEntry.Error, "some-error")
	}
	if gotEntry.Baseline.Evicted != baseline.Evicted || gotEntry.Baseline.Moved != baseline.Moved {
		t.Fatalf("Baseline mismatch: got %+v, want %+v", gotEntry.Baseline, baseline)
	}
	// Compare PlacedByPriority maps manually since maps cannot be compared directly
	if len(gotEntry.Baseline.PlacedByPriority) != len(baseline.PlacedByPriority) {
		t.Fatalf("Baseline.PlacedByPriority length mismatch: got %d, want %d", len(gotEntry.Baseline.PlacedByPriority), len(baseline.PlacedByPriority))
	}
	for k, v := range baseline.PlacedByPriority {
		if gotEntry.Baseline.PlacedByPriority[k] != v {
			t.Fatalf("Baseline.PlacedByPriority[%q] = %d, want %d", k, gotEntry.Baseline.PlacedByPriority[k], v)
		}
	}
	if len(gotEntry.Attempts) != len(attempts) {
		t.Fatalf("Attempts len = %d, want %d", len(gotEntry.Attempts), len(attempts))
	}
	if gotEntry.Attempts[0].Name != "python" || gotEntry.Attempts[0].Status != "OPTIMAL" {
		t.Fatalf("summarized Attempts[0] = %#v, want Name=python Status=OPTIMAL", gotEntry.Attempts[0])
	}
	if gotEntry.TimestampNs == 0 {
		t.Fatalf("TimestampNs not set")
	}
}

// -------------------------
// appendSolverStatsCM
// --------------------------

func TestAppendSolverStatsCM_NoClientSet_SkipsWithoutPanic(t *testing.T) {
	ctx := context.Background()
	pl := &SharedState{
		Handle: &fakeHandle{
			client:  nil,
			factory: nil,
		},
	}

	// Ensure hook is disabled so we execute the real body.
	orig := appendSolverStatsCMHook
	appendSolverStatsCMHook = nil
	defer func() { appendSolverStatsCMHook = orig }()

	// Just ensure it doesn't panic when there is no clientset.
	pl.appendSolverStatsCM(ctx, ExportedSolverStats{BestName: "best"})
}

func TestAppendSolverStatsCM_CreatesConfigMapOnNotFound(t *testing.T) {
	ctx := context.Background()

	// Start with an empty fake cluster.
	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)

	pl := &SharedState{
		Handle: &fakeHandle{
			client:  client,
			factory: factory,
		},
	}

	// Make sure we go through the real implementation, not the hook.
	orig := appendSolverStatsCMHook
	appendSolverStatsCMHook = nil
	defer func() { appendSolverStatsCMHook = orig }()

	entry := ExportedSolverStats{
		BestName: "python",
		// other fields not strictly necessary for this test
	}

	pl.appendSolverStatsCM(ctx, entry)

	// After appendSolverStatsCM, we expect the ConfigMap to exist.
	cm, err := client.CoreV1().
		ConfigMaps(SystemNamespace).
		Get(ctx, SolverStatsConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected stats ConfigMap to be created, got err = %v", err)
	}

	dataKey := SolverStatsConfigMapLabelKey + ".json"
	payload, ok := cm.Data[dataKey]
	if !ok || payload == "" {
		t.Fatalf("expected non-empty JSON payload in key %q, got %q", dataKey, payload)
	}
}
