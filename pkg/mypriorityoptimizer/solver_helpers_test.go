package mypriorityoptimizer

import (
	"errors"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func hasKey(args []any, key string) bool {
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok && k == key {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// isAnySolverEnabled
// -----------------------------------------------------------------------------

func TestIsAnySolverEnabled(t *testing.T) {
	origPy := SolverPythonEnabled
	origBfs := SolverBfsEnabled
	origLocal := SolverLocalSearchEnabled
	defer func() {
		SolverPythonEnabled = origPy
		SolverBfsEnabled = origBfs
		SolverLocalSearchEnabled = origLocal
	}()

	SolverPythonEnabled = false
	SolverBfsEnabled = false
	SolverLocalSearchEnabled = false
	pl := &SharedState{}

	if got := pl.isAnySolverEnabled(); got {
		t.Fatalf("isAnySolverEnabled() with all solvers disabled = %v, want false", got)
	}

	SolverPythonEnabled = true
	if got := pl.isAnySolverEnabled(); !got {
		t.Fatalf("isAnySolverEnabled() with python enabled = %v, want true", got)
	}

	SolverPythonEnabled = false
	SolverBfsEnabled = true
	if got := pl.isAnySolverEnabled(); !got {
		t.Fatalf("isAnySolverEnabled() with bfs enabled = %v, want true", got)
	}

	SolverBfsEnabled = false
	SolverLocalSearchEnabled = true
	if got := pl.isAnySolverEnabled(); !got {
		t.Fatalf("isAnySolverEnabled() with local search enabled = %v, want true", got)
	}
}

// -----------------------------------------------------------------------------
// buildSolverInput (error branch only, no usable nodes)
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// buildBaselineScore
// -----------------------------------------------------------------------------

func TestBuildBaselineScore(t *testing.T) {
	in := SolverInput{
		Pods: []SolverPod{
			{Priority: 1, Node: "n1"}, // placed
			{Priority: 1, Node: ""},   // pending
			{Priority: 2, Node: "n2"}, // placed
		},
	}
	score := buildBaselineScore(in)

	if score.Evicted != 0 || score.Moved != 0 {
		t.Fatalf("baseline score Evicted/Moved = (%d,%d), want (0,0)", score.Evicted, score.Moved)
	}
	if got := score.PlacedByPriority["1"]; got != 1 {
		t.Fatalf("PlacedByPriority['1'] = %d, want 1", got)
	}
	if got := score.PlacedByPriority["2"]; got != 1 {
		t.Fatalf("PlacedByPriority['2'] = %d, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// solverConfigArgs
// -----------------------------------------------------------------------------

func TestSolverConfigArgs(t *testing.T) {
	origPy := SolverPythonEnabled
	origBfs := SolverBfsEnabled
	origLocal := SolverLocalSearchEnabled
	origHints := SolverUseHints
	origSave := SolverSaveAllAttempts
	defer func() {
		SolverPythonEnabled = origPy
		SolverBfsEnabled = origBfs
		SolverLocalSearchEnabled = origLocal
		SolverUseHints = origHints
		SolverSaveAllAttempts = origSave
	}()

	// Case 1: all solvers disabled
	SolverPythonEnabled = false
	SolverBfsEnabled = false
	SolverLocalSearchEnabled = false
	SolverUseHints = true
	SolverSaveAllAttempts = false

	args := solverConfigArgs()
	if hasKey(args, "pythonSolver") || hasKey(args, "bfsSolver") || hasKey(args, "localSearchSolver") {
		t.Fatalf("solverConfigArgs() should not contain solver keys when all disabled, got %v", args)
	}
	if !hasKey(args, "useHints") || !hasKey(args, "saveFailedAttempts") {
		t.Fatalf("solverConfigArgs() must always include shared flags, got %v", args)
	}

	// Case 2: python only
	SolverPythonEnabled = true
	SolverBfsEnabled = false
	SolverLocalSearchEnabled = false

	args = solverConfigArgs()
	if !hasKey(args, "pythonSolver") {
		t.Fatalf("solverConfigArgs() missing pythonSolver when python enabled, got %v", args)
	}
	if hasKey(args, "bfsSolver") || hasKey(args, "localSearchSolver") {
		t.Fatalf("solverConfigArgs() should not contain other solver keys, got %v", args)
	}
}

// -----------------------------------------------------------------------------
// isImprovement / comparePlaced / cmpInt
// -----------------------------------------------------------------------------

func TestComparePlaced_HighPriorityWins(t *testing.T) {
	a := map[string]int{"1": 2, "0": 1}
	b := map[string]int{"1": 1, "0": 5}

	if got := comparePlaced(a, b); got != 1 {
		t.Fatalf("comparePlaced(a,b) = %d, want 1 (a better high-prio)", got)
	}
	if got := comparePlaced(b, a); got != -1 {
		t.Fatalf("comparePlaced(b,a) = %d, want -1 (b worse high-prio)", got)
	}

	// Equal maps
	if got := comparePlaced(a, map[string]int{"1": 2, "0": 1}); got != 0 {
		t.Fatalf("comparePlaced(equal) = %d, want 0", got)
	}
}

func TestCmpInt(t *testing.T) {
	if got := cmpInt(1, 2); got != 1 {
		t.Fatalf("cmpInt(1,2) = %d, want 1 (improvement)", got)
	}
	if got := cmpInt(3, 2); got != -1 {
		t.Fatalf("cmpInt(3,2) = %d, want -1 (worse)", got)
	}
	if got := cmpInt(2, 2); got != 0 {
		t.Fatalf("cmpInt(2,2) = %d, want 0 (equal)", got)
	}
}

func TestIsImprovement_Order(t *testing.T) {
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
	if got := isImprovement(base, suggBetterPlaced); got != 1 {
		t.Fatalf("isImprovement() placed better = %d, want 1", got)
	}

	// Same placed, fewer evictions
	suggBetterEvict := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          1,
		Moved:            3,
	}
	if got := isImprovement(base, suggBetterEvict); got != 1 {
		t.Fatalf("isImprovement() fewer evictions = %d, want 1", got)
	}

	// Same placed/evictions, more moves (worse)
	suggMoreMoves := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          2,
		Moved:            4,
	}
	if got := isImprovement(base, suggMoreMoves); got != -1 {
		t.Fatalf("isImprovement() more moves = %d, want -1", got)
	}

	// Exactly equal
	same := SolverScore{
		PlacedByPriority: map[string]int{"1": 1, "0": 1},
		Evicted:          2,
		Moved:            3,
	}
	if got := isImprovement(base, same); got != 0 {
		t.Fatalf("isImprovement() equal = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// hasSolverFeasibleResult
// -----------------------------------------------------------------------------

func TestHasSolverFeasibleResult(t *testing.T) {
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
		if got := hasSolverFeasibleResult(tt.status); got != tt.want {
			t.Fatalf("hasSolverFeasibleResult(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

// -----------------------------------------------------------------------------
// summarizeAttempt
// -----------------------------------------------------------------------------

func TestSummarizeAttempt_UsesExistingStatusIfSet(t *testing.T) {
	r := SolverResult{
		Name:       "python",
		Status:     "FAILED",
		DurationMs: 10,
		Score: SolverScore{
			PlacedByPriority: map[string]int{"1": 1},
		},
		Output: &SolverOutput{Status: "OPTIMAL"},
	}

	s := summarizeAttempt(r)
	if s.Status != "FAILED" {
		t.Fatalf("summarizeAttempt() Status = %q, want %q", s.Status, "FAILED")
	}
	if s.Name != r.Name || s.DurationMs != r.DurationMs {
		t.Fatalf("summarizeAttempt() changed fields unexpectedly: %#v", s)
	}
}

func TestSummarizeAttempt_DerivesStatusFromOutput(t *testing.T) {
	r := SolverResult{
		Name:       "python",
		Status:     "",
		DurationMs: 5,
		Output:     &SolverOutput{Status: "OPTIMAL"},
	}
	s := summarizeAttempt(r)
	if s.Status != "OPTIMAL" {
		t.Fatalf("summarizeAttempt() Status = %q, want %q", s.Status, "OPTIMAL")
	}
}

// -----------------------------------------------------------------------------
// computeSolverScore (minimal branch: out == nil)
// -----------------------------------------------------------------------------

func TestComputeSolverScore_NilOutput(t *testing.T) {
	in := SolverInput{
		Pods: []SolverPod{
			{UID: "u1", Priority: 1, Node: "n1"},
		},
	}
	score := computeSolverScore(in, nil)
	if len(score.PlacedByPriority) != 0 || score.Evicted != 0 || score.Moved != 0 {
		t.Fatalf("computeSolverScore() with nil out = %#v, want zero score", score)
	}
}

// -----------------------------------------------------------------------------
// toSolverPod
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// planApplicable (branches that don't require knowing SolverOutput element types)
// -----------------------------------------------------------------------------

func TestPlanApplicable_NilPlan(t *testing.T) {
	pl := &SharedState{}
	ok, reason := pl.planApplicable(nil, nil, nil)
	if ok {
		t.Fatalf("planApplicable(nil, ...) = true, want false")
	}
	if reason != "nil plan" {
		t.Fatalf("planApplicable(nil, ...) reason = %q, want %q", reason, "nil plan")
	}
}

func TestPlanApplicable_CapacityExceededWhenNoNodes(t *testing.T) {
	pl := &SharedState{}

	// One running pod on some node, but we pass *no* nodes to planApplicable.
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

	ok, reason := pl.planApplicable(out, nil, []*v1.Pod{pod})
	if ok {
		t.Fatalf("planApplicable() with used resources but no node capacities = true, want false")
	}
	if !strings.Contains(reason, "capacity exceeded") {
		t.Fatalf("planApplicable() reason = %q, want it to contain 'capacity exceeded'", reason)
	}
}

// -----------------------------------------------------------------------------
// exportSolverStatsConfigMap / appendSolverStatsCM and logLeaderboard
// -----------------------------------------------------------------------------
// These involve real Kubernetes clients and klog output. They are better
// exercised in higher-level integration tests, so we deliberately keep
// unit tests focused on the pure logic above.
