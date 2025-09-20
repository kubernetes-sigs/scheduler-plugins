// types.go

package mycrossnodepreemption

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// ===== Plugin root =====

// MyCrossNodePreemption is the main plugin struct.
type MyCrossNodePreemption struct {
	// Handle to the framework
	Handle framework.Handle
	// Kubernetes client
	Client kubernetes.Interface
	// Whether a plan is active
	Active atomic.Bool
	// Currently active plan (if any)
	ActivePlan atomic.Pointer[ActivePlan]
	// Set of blocked pods
	Blocked *PodSet
	// Set of batched pods
	Batched *PodSet
	// Mutex to wait for caches to warm up
	CachesWarm atomic.Bool
	// Ensures loops start exactly once after caches warm
	LoopsStarted atomic.Bool
}

// ===== Plan types =====

// StoredPlan represents the plan to be executed.
type StoredPlan struct {
	// Plugin version that generated the plan
	PluginVersion string `json:"pluginVersion"`
	// The optimization mode used
	OptimizationStrategy string `json:"optimizationStrategy"`
	// When the plan was generated
	GeneratedAt time.Time `json:"generatedAt"`
	// When the plan was completed (if ever)
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	// Status of the plan
	Status PlanStatus `json:"status"`
	// Single-preemptor metadata
	Preemptor *Preemptor `json:"preemptor,omitempty"`
	// Evicted pods
	Evicts []Placement `json:"evicts,omitempty"`
	// Moved pods
	Moves []NewPlacement `json:"moves,omitempty"`
	// Solver summary (status & score)
	Solver SolverSummary `json:"solver"`
	// All pods and their old placements
	OldPlacements []Placement `json:"oldPlacements,omitempty"`
	// All pods and their new placements
	NewPlacement []NewPlacement `json:"newPlacement,omitempty"`
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string `json:"placementsByName,omitempty"`
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotasDoc WorkloadQuotas `json:"workloadQuotas,omitempty"`
}

// PlanStatus represents the status of a plan.
type PlanStatus string

const (
	PlanStatusActive    PlanStatus = "Active"
	PlanStatusCompleted PlanStatus = "Completed"
	PlanStatusFailed    PlanStatus = "Failed"
)

// ===== Runtime types =====

// ActivePlan represents the state of an active plan.
type ActivePlan struct {
	// Unique plan ID
	ID string
	// Workload quotas for moved or new pods that are part of a workload
	WorkloadPerNodeCnts WorkloadQuotasAtomics
	// Placements by name for standalone pods that is either moved or new: ns/name -> node
	PlacementByName map[string]string // pod ns/name -> targetNode
	// Context to cancel the plan
	Ctx context.Context
	// Cancel function to cancel the plan
	Cancel context.CancelFunc
}

// ===== Workload types =====

// WorkloadQuotas is a map of workloadKey -> node -> remaining count
type WorkloadQuotas map[string]map[string]int32

// WorkloadQuotasAtomics is a map of workloadKey -> node -> remaining count
// The atomic.Int32 allows concurrent safe decrement during plan execution.
type WorkloadQuotasAtomics map[string]map[string]*atomic.Int32

// WorkloadKind represents the kind of workload.
type WorkloadKind int

const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

// WorkloadKey is a key to identify a workload.
type WorkloadKey struct {
	// What kind of workload
	Kind WorkloadKind
	// Namespace of the workload
	Namespace string
	// Name of the workload
	Name string
}

// ===== Solver types =====

// SolveMode indicates the mode of solving.
type SolveMode int

const (
	SolveBatch SolveMode = iota
	SolveSingle
	SolveContinuous
)

const (
	SolverModeLexi     = "lexi"
	SolverModeWeighted = "weighted"
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
	// Optimization mode: "lexi" or "weighted"
	Mode string `json:"solver_mode,omitempty"`
	// Whether to use hints
	UseHints bool `json:"use_hints,omitempty"`
	// Thresholds/targets to improve from
	Hints *Score `json:"hints,omitempty"`
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

// SolverSummary summarizes the result of a solver attempt.
type SolverSummary struct {
	// Name of the solver used
	Name string `json:"name,omitempty"`
	// Status of the solver
	Status string `json:"status,omitempty"`
	// DurationUs of the solver
	DurationUs int64 `json:"durationUs,omitempty"`
	// Score of the solution
	Score Score `json:"score,omitempty"`
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

// ===== Building blocks =====

// Score of a solver solution
type Score struct {
	// Number of pods placed by priority (higher is better)
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	// Number of evicted pods (lower is better)
	Evicted int `json:"evicted,omitempty"`
	// Number of moved pods (lower is better)
	Moved int `json:"moved,omitempty"`
}

// Pod represents a pod with minimal info.
type Pod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid,omitempty"`
	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`
	// Name of the pod
	Name string `json:"name,omitempty"`
}

// NewPlacement represents a pod placement from one node to another.
type NewPlacement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Previous node of the pod; empty if new pod
	FromNode string `json:"fromNode,omitempty"`
	// New node of the pod
	ToNode string `json:"toNode"`
}

// Placement represents a pod placement on a node.
type Placement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Node the pod is placed on
	Node string `json:"node"`
}

// Preemptor represents a preemptor pod and its nominated node.
type Preemptor struct {
	// Pod being preempted
	Pod Pod `json:"pod"`
	// Nominated node for the preemptor pod
	NominatedNode string `json:"nominatedNode"`
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

// ===== misc =====

// FlowResult represents the result of a scheduling flow.
type FlowResult struct {
	// ID of the plan (if any)
	PlanID string
	// Target node of the preemptor (if any)
	TargetNode string
	// Batch size (if any)
	BatchSize int
	// Total pods before plan execution
	TotalPrePlan int
	// Total pods after plan execution
	TotalPostPlan int
	// Chosen solver
	ChosenSolver SolverSummary
	// Total duration of the flow
	TotalDuration time.Duration
}

// ===== PodSet =====

// PodSet is a thread-safe set of pods.
type PodSet struct {
	// mu protects the map
	mu sync.RWMutex
	// m maps pod UID to PodKey
	m map[types.UID]PodKey
}

// PodKey is a minimal key for identifying a pod.
type PodKey struct {
	// Unique identifier for the pod
	UID types.UID // for fast lookup, we only use types.UID as key (not as a string)
	// Namespace of the pod
	Namespace string
	// Name of the pod
	Name string
}
