package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// PlannerInput is the input to a planner.
type PlannerInput struct {
	// Preemptor pod (if any; single pod mode)
	Preemptor *PlannerPod `json:"preemptor,omitempty"`
	// Nodes to consider
	Nodes []PlannerNode `json:"nodes"`
	// Pods to consider
	Pods []PlannerPod `json:"pods"`
	// Baseline score for comparison
	BaselineScore PlannerScore `json:"baseline_score"`
	// Timeout for the solver (ms)
	TimeoutMs int64 `json:"timeout_ms"`
	// If true, ignore affinity rules
	IgnoreAffinity bool `json:"ignore_affinity"`
}

// PlannerOutput is the output from a solver.
type PlannerOutput struct {
	// Status of the planner
	Status string `json:"status"`
	// Pod placements (new or moved)
	Placements []PlannerPod `json:"placements"`
	// Evicted pods
	Evictions []PlannerPod `json:"evictions"`
	// Duration in milliseconds of the planner
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// PlannerNode represents a node in the cluster for the solver.
type PlannerNode struct {
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
	Pods map[types.UID]*PlannerPod `json:"-"`
}

// PlannerPod represents a pod in the cluster.
type PlannerPod struct {
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

// PlannerAttempt defines a planner attempt configuration and function.
type PlannerAttempt struct {
	// Name of the planner attempt
	Name string
	// Whether the planner attempt is enabled
	Enabled bool
	// Timeout for the planner attempt
	Timeout time.Duration
	// Function to run the planner attempt
	Run func(ctx context.Context, in PlannerInput) (*PlannerOutput, error)
}

// PlannerResult is the result of a planner attempt.
type PlannerResult struct {
	// Name of the planner attempt
	Name string `json:"name,omitempty"`
	// Status of the planner
	// filled from Output.Status when present
	Status string `json:"status,omitempty"`
	// DurationMs of the planner
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Score of the solution
	Score PlannerScore `json:"score,omitempty"`
	// Stages of the planner
	Stages []SolverPhase `json:"stages,omitempty"`
}

// PlannerScore of a planner solution
type PlannerScore struct {
	// Number of pods placed by priority (higher is better)
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	// Number of evicted pods (lower is better)
	Evicted int `json:"evicted,omitempty"`
	// Number of moved pods (lower is better)
	Moved int `json:"moved,omitempty"`
}

// ExportedPlannerStats is the structure used to export planner run statistics.
type ExportedPlannerStats struct {
	// TimestampNs is the timestamp of the run in nanoseconds.
	TimestampNs int64 `json:"timestamp_ns"`
	// Error (if any)
	Error string `json:"error,omitempty"`
	// Best solver name
	BestName string `json:"best_name,omitempty"`
	// Baseline score
	Baseline PlannerScore `json:"baseline,omitempty"`
	// Best score
	Attempts []PlannerResult `json:"attempts,omitempty"`
}

type Plan struct {
	// Evicted pods
	Evicts []PlannerPod `json:"evicts"`
	// Moved pods
	Moves []PlannerPod `json:"moves"`
	// All pods and their old placements
	OldPlacements []PlannerPod `json:"old_placements"`
	// All pods and their new placements
	NewPlacements []PlannerPod `json:"new_placements"`
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string `json:"placement_by_name"`
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotas WorkloadQuotas `json:"workload_quotas"`
	// Nominated node for the preemptor pod (if any)
	NominatedNode string `json:"nominated_node,omitempty"`
}

// StoredPlan represents the plan to be executed.
type StoredPlan struct {
	// Plugin version that generated the plan
	PluginVersion string `json:"plugin_version"`
	// The optimization mode used
	OptimizationStrategy string `json:"optimization_strategy"`
	// When the plan was generated
	GeneratedAt time.Time `json:"generated_at"`
	// When the plan was completed (if ever)
	CompletedAt time.Time `json:"completed_at,omitempty"`
	// SolverResult summary (status & score)
	SolverResult PlannerResult `json:"solver_result"`
	// PlanStatus of the plan
	PlanStatus PlanStatus `json:"plan_status"`
	// The actual plan
	Plan *Plan `json:"plan"`
}

// PlanStatus represents the status of a plan.
type PlanStatus string

const (
	// Active status indicates the plan is currently being executed.
	PlanStatusActive PlanStatus = "Active"
	// Inactive status indicates the plan is not currently being executed.
	PlanStatusCompleted PlanStatus = "Completed"
	// Failed status indicates the plan execution failed.
	PlanStatusFailed PlanStatus = "Failed"
)

// WorkloadQuotas is a map of workloadKey -> node -> remaining count
type WorkloadQuotas map[string]map[string]int32
