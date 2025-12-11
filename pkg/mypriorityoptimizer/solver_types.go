// solver_types.go
package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// SolverInput is the input to a solver.
type SolverInput struct {
	// Preemptor pod (if any; single pod mode)
	Preemptor *SolverPod `json:"preemptor,omitempty"`
	// Nodes to consider
	Nodes []SolverNode `json:"nodes"`
	// Pods to consider
	Pods []SolverPod `json:"pods"`
	// Baseline score for comparison
	BaselineScore SolverScore `json:"baseline_score"`
	// Timeout for the solver (ms)
	TimeoutMs int64 `json:"timeout_ms"`
	// If true, ignore affinity rules
	IgnoreAffinity bool `json:"ignore_affinity"`
}

// SolverOutput is the output from a solver.
type SolverOutput struct {
	// Status of the solver
	Status string `json:"status"`
	// Pod placements (new or moved)
	Placements []SolverPod `json:"placements"`
	// Evicted pods
	Evictions []SolverPod `json:"evictions"`
	// Duration in milliseconds of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`
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

// SolverPod represents a pod in the cluster.
type SolverPod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid,omitempty"`
	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`
	// Name of the pod
	Name string `json:"name,omitempty"`
	// Requested CPU in millicores
	ReqCPUm int64 `json:"req_cpu_m,omitempty"`
	// Requested memory in bytes
	ReqMemBytes int64 `json:"req_mem_bytes,omitempty"`
	// Priority of the pod
	Priority int32 `json:"priority,omitempty"`
	// Whether the pod is protected from preemption
	Protected bool `json:"protected,omitempty"`
	// Current node of the pod (empty if new pod)
	Node string `json:"node,omitempty"`
	// Old node of the pod (empty if new pod)
	OldNode string `json:"old_node,omitempty"`
}

// SolverAttempt defines a solver attempt configuration and function.
type SolverAttempt struct {
	// Name of the solver attempt
	Name string
	// Whether the solver attempt is enabled
	Enabled bool
	// Timeout for the solver attempt
	Timeout time.Duration
	// Function to run the solver attempt
	Run func(ctx context.Context, in SolverInput) (*SolverOutput, error)
}

// SolverResult is the result of a solver attempt.
type SolverResult struct {
	// Name of the solver attempt
	Name string `json:"name,omitempty"`
	// Status of the solver attempt
	// filled from Output.Status when present
	Status string `json:"status,omitempty"`
	// DurationMs of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Score of the solution
	Score SolverScore `json:"score,omitempty"`
	// Stages of the solver
	Stages []SolverPhase `json:"stages,omitempty"`
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

// ExportedSolverStats is the structure used to export solver run statistics.
type ExportedSolverStats struct {
	// TimestampNs is the timestamp of the run in nanoseconds.
	TimestampNs int64 `json:"timestamp_ns"`
	// Error (if any)
	Error string `json:"error,omitempty"`
	// Best solver name
	BestName string `json:"best_name,omitempty"`
	// Baseline score
	Baseline SolverScore `json:"baseline,omitempty"`
	// Best score
	Attempts []SolverResult `json:"attempts,omitempty"`
}

// SolverPhase represents a phase/stage of the solver process.
type SolverPhase struct {
	// Tier of the solver stage (0 = placement, 1..n = moves)
	Tier int `json:"tier"`
	// Name of the solver stage (e.g. "place" vs "moves")
	Stage string `json:"stage"`
	// Status of the solver stage
	Status string `json:"status"`
	// Duration of the solver stage
	DurationMs int64 `json:"duration_ms"`
	// The ratio gap to optimality (if known)
	RelativeGap string `json:"relative_gap,omitempty"`
}
