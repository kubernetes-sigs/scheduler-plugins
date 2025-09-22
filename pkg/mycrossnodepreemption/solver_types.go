// solver_types.go

package mycrossnodepreemption

import (
	"context"
	"math/rand"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// SolveMode indicates the mode of solving.
type SolveMode int

const (
	SolveAll SolveMode = iota
	SolveSingle
)

// SolverInput is the input to a solver.
type SolverInput struct {
	// Preemptor pod (if any; single pod mode)
	Preemptor *SolverPod `json:"preemptor,omitempty"`
	// Nodes to consider
	Nodes []SolverNode `json:"nodes"`
	// Pods to schedule / re-schedule
	Pods []SolverPod `json:"pods"`
	// Timeout for the solver (ms)
	TimeoutMs int64 `json:"timeout_ms"`
	// If true, ignore affinity rules
	IgnoreAffinity bool `json:"ignore_affinity"`
	// Log solver progress
	LogProgress bool `json:"log_progress,omitempty"`
	// Whether to use hints
	UseHints bool `json:"use_hints,omitempty"`
	// Thresholds/targets to improve from
	Hints *SolverScore `json:"hints,omitempty"`
	// Number of parallel workers (0 = auto)
	Workers int `json:"workers,omitempty"`
	// Maximum number of trials (0 = unlimited)
	MaxTrials int `json:"max_trials,omitempty"` // for local search
}

// SolverOutput is the output from a solver.
type SolverOutput struct {
	// Status of the solver (failed/infeasible/feasible/optimal)
	Status string `json:"status"`
	// Pod placements (new or moved)
	Placements []NewPlacement `json:"placements"`
	// Evicted pods
	Evictions []Placement `json:"evictions"`
}

// SolverNode represents a node in the cluster for the solver.
type SolverNode struct {
	// Name of the node
	Name string `json:"name"`
	// Total CPU capacity in millicores
	CapCPUm int64 `json:"cap_cpu_m"`
	// Total memory capacity in bytes
	CapMemBytes int64 `json:"cap_mem_bytes"`
	// Allocated (used) CPU in millicores (not serialized)
	AllocCPUm int64 `json:"-"`
	// Allocated (used) memory in bytes (not serialized)
	AllocMemBytes int64 `json:"-"`
	// Labels of the node
	Labels map[string]string `json:"labels,omitempty"`
	// Pods currently on the node (not serialized)
	Pods map[types.UID]*SolverPod `json:"-"`
}

// SolverPod represents a pod for the solver.
type SolverPod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid"`
	// Namespace of the pod
	Namespace string `json:"namespace"`
	// Name of the pod
	Name string `json:"name"`
	// Requested CPU in millicores
	ReqCPUm int64 `json:"req_cpu_m"`
	// Requested memory in bytes
	ReqMemBytes int64 `json:"req_mem_bytes"`
	// Priority of the pod
	Priority int32 `json:"priority"`
	// Whether the pod is protected from preemption
	Protected bool `json:"protected,omitempty"`
	// Current node of the pod (empty if new pod)
	Node string `json:"node"`
}

// SolverAttempt defines a solver attempt configuration and function.
type SolverAttempt struct {
	// Name of the solver attempt
	Name string
	// Whether the solver attempt is enabled
	Enabled bool
	// Timeout for the solver attempt
	Timeout time.Duration
	// Number of solver trials to try
	Trials int
	// Time that the solver is allowed to exceed the timeout (ms)
	// Can be used to let the solver provide a final solution after timeout.
	FudgeMs int64
	// Function to run the solver attempt
	Run func(ctx context.Context, in SolverInput) (*SolverOutput, error)
}

// SolverResult is the result of a solver attempt.
type SolverResult struct {
	// Name of the solver attempt
	Name string `json:"name,omitempty"`
	// Status of the solver
	// filled from Output.Status when present
	Status string `json:"status,omitempty"`
	// DurationUs of the solver
	DurationUs int64 `json:"durationUs,omitempty"`
	// Score of the solution
	Score SolverScore `json:"score,omitempty"`

	// In-memory only (not exported)
	// Comparison vs previous leader (-1 worse, 0 tie, 1 better)
	CmpBase int `json:"-"`
	// Full detailed solver output (not exported)
	Output *SolverOutput `json:"-"`
}

// SolverScore of a solver solution
type SolverScore struct {
	// Number of pods placed by priority (higher is better)
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	// Number of evicted pods (lower is better)
	Evicted int `json:"evicted,omitempty"`
	// Number of moved pods (lower is better)
	Moved int `json:"moved,omitempty"`
}

// PreparedState is the prepared state for solving.
type PreparedState struct {
	// Whether we are in single-preemptor mode
	Single bool
	// Nodes to consider
	Nodes map[string]*SolverNode
	// Pods to schedule / re-schedule
	Pods map[types.UID]*SolverPod
	// Ordered list of nodes (by available resources descending)
	Order []*SolverNode
	// Current resource deltas per node (CPU, MEM)
	Preemptor *SolverPod
	// The list of pods to work on (sorted by priority desc, req desc)
	Worklist []*SolverPod
	// Move gate for local search
	MoveGate *int32
}

// PlanFunc is a function that, given a pod and a target node, tries to find a plan.
type PlanFunc func(
	pending *SolverPod,
	target *SolverNode,
	nodes map[string]*SolverNode,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[types.UID]struct{},
	trial int,
	rng *rand.Rand,
) ([]MoveLite, bool)

// Light representation of a pod move.
type MoveLite struct {
	// Unique identifier for the pod
	UID types.UID
	// Source node of the pod
	From string
	// Target node of the pod
	To string
}

// TargetScore is used to order nodes by how well they can accommodate a pod.
type TargetScore struct {
	// Node is the solver node being scored.
	Node *SolverNode
	// Computed score values.
	// max(defCPU/p.CPU, defMEM/p.MEM)
	Score float64
	// Deficit in CPU and Memory if placed on this node.
	DefSum int64
	// Waste in CPU and Memory if placed on this node.
	Waste int64
}

// VictimOptions holds options for getVictims.
// TODO
type VictimOptions struct {
	Strategy     VictimStrategy
	MoveGate     *int32                 // priority gate for moves
	NeedCPU      int64                  // remaining CPU deficit on the active node
	NeedMem      int64                  // remaining MEM deficit on the active node
	Cap          int                    // max victims to return (0 = no cap)
	Order        []*SolverNode          // required for VictimsLocal (to compute relocCount)
	MovedUIDs    map[types.UID]struct{} // prefer already-moved in local
	Rng          *rand.Rand             // for randomization (nil = none)
	RandomizePct int                    // % of randomization of victim order (0 = none)
}

// TODO
type VictimStrategy int

// TODO
const (
	VictimsBFS   VictimStrategy = iota // coverage-first for BFS
	VictimsLocal                       // relocatability-aware for local search
)

// Delta represents a change in CPU and Memory.
type Delta struct {
	// Delta in CPU (millicores)
	CPU int64
	// Delta in Memory (bytes)
	Mem int64
}

// ExportedSolverStats is the structure used to export solver run statistics.
type ExportedSolverStats struct {
	// TimestampNs is the timestamp of the run in nanoseconds.
	TimestampNs int64 `json:"timestamp_ns"`
	// Best solver name
	Best string `json:"best,omitempty"`
	// Plan status
	PlanStatus PlanStatus `json:"plan_status,omitempty"`
	// Baseline score
	Baseline SolverScore `json:"baseline"`
	// Best score
	Attempts []SolverResult `json:"attempts"`
}
