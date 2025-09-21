// solver_types.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// TODO: Reach to here in this file...

// SolveMode indicates the mode of solving.
type SolveMode int

const (
	SolveBatch SolveMode = iota
	SolveSingle
	SolveContinuous
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

// SolverSummary summarizes the result of a solver attempt.
type SolverSummary struct {
	// Name of the solver used
	Name string `json:"name,omitempty"`
	// Status of the solver
	Status string `json:"status,omitempty"`
	// DurationUs of the solver
	DurationUs int64 `json:"durationUs,omitempty"`
	// Score of the solution
	Score SolverScore `json:"score,omitempty"`
}

// SolverAttempt defines a solver attempt configuration and function.
type SolverAttempt struct {
	// Name of the solver attempt
	Name string
	// Whether the solver attempt is enabled
	Enabled bool
	// Timeout for the solver attempt
	Timeout time.Duration
	// Number of trials (for local search)
	Trials int
	// Time that the solver is allowed to exceed the timeout (ms)
	// Can be used to let the solver provide a final solution after timeout.
	FudgeMs int64
	// Function to run the solver attempt
	Run func(ctx context.Context, in SolverInput) (*SolverOutput, error)
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
