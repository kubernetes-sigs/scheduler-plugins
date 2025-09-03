// pkg/mycrossnodepreemption/types.go
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

type MyCrossNodePreemption struct {
	Handle     framework.Handle
	Client     kubernetes.Interface
	Active     atomic.Bool
	ActivePlan atomic.Pointer[ActivePlanState]
	Blocked    *PodSet
	Batched    *PodSet
	CachesWarm atomic.Bool
}

// ===== Workload identification =====

type WorkloadKind int

const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

type WorkloadKey struct {
	Kind      WorkloadKind
	Namespace string
	Name      string
}

// ===== Optimization modes =====

type OptimizationCadenceMode int

const (
	OptimizeForEvery OptimizationCadenceMode = iota
	OptimizeInBatches
	OptimizeContinuously
)

type OptimizationAtMode int

const (
	OptimizeAtPreEnqueue OptimizationAtMode = iota
	OptimizeAtPostFilter
)

type SolveMode int

const (
	SolveBatch SolveMode = iota
	SolveSingle
	SolveContinuously
)

type StrategyDecision int

const (
	DecidePassThrough StrategyDecision = iota
	DecideBatch
	DecideEvery
	DecideBlockActive
)

type Phase string

const (
	PhasePreEnqueue Phase = "PreEnqueue"
	PhasePostFilter Phase = "PostFilter"
	PhaseBatch      Phase = "BatchLoop"
	PhaseContinuous Phase = "ContinuousLoop"
)

// ===== Solver I/O (unchanged, just kept) =====

const (
	SolverModeLexi     = "lexi"
	SolverModeWeighted = "weighted"
)

type SolverInput struct {
	Preemptor      *SolverPod   `json:"preemptor,omitempty"`
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	LogProgress    bool         `json:"log_progress,omitempty"`
	Mode           string       `json:"solver_mode,omitempty"`
	UseHints       bool         `json:"use_hints,omitempty"`
	Workers        int          `json:"workers,omitempty"`
}

type SolverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU_m     int64  `json:"cpu_m"`
	MemBytes  int64  `json:"mem_bytes"`
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`
	Protected bool   `json:"protected,omitempty"`
}

type SolverNode struct {
	Name     string            `json:"name"`
	CPUm     int64             `json:"cpu_m"`
	MemBytes int64             `json:"mem_bytes"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type SolverOutput struct {
	Status     string            `json:"status"`
	Placements map[string]string `json:"placements"` // uid -> targetNode
	Evictions  []SolverEviction  `json:"evictions"`
}

type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type SolverSummary struct {
	Status string `json:"status"`
	Score  Score  `json:"score,omitempty"`
}

// ===== Building blocks =====

type Score struct {
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	Evicted          int            `json:"evicted,omitempty"`
	Moved            int            `json:"moved,omitempty"`
}

type PodLite struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type NewPlacements struct {
	Pod        PodLite `json:"pod"`
	TargetNode string  `json:"targetNode"`
}

type OldPlacements struct {
	Pod  PodLite `json:"pod"`
	Node string  `json:"node"`
}

type MoveLite struct {
	Pod      PodLite `json:"pod"`
	FromNode string  `json:"fromNode"`
	ToNode   string  `json:"toNode"`
}

type EvictLite struct {
	Pod      PodLite `json:"pod"`
	FromNode string  `json:"fromNode"`
}

type Preemtor struct {
	Pod           PodLite `json:"pod"`
	NominatedNode string  `json:"nominatedNode"`
}

// WorkloadPerNode: workloadKey -> node -> desired count
type WorkloadPerNode map[string]map[string]int

// ===== Your new StoredPlan shape =====

type StoredPlan struct {
	PluginVersion string     `json:"pluginVersion"`
	Mode          string     `json:"mode"`
	GeneratedAt   time.Time  `json:"generatedAt"`
	Completed     bool       `json:"completed"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	Preemptor     *Preemtor  `json:"preemptor,omitempty"` // Single-preemptor metadata (nil in batch/continuous)
	// Actions
	Evicts []EvictLite `json:"evicts,omitempty"`
	Moves  []MoveLite  `json:"moves,omitempty"`
	// Solver summary (status & score)
	Solver SolverSummary `json:"solver"`
	// Reference snapshot (where pods were at solve time) uid -> node
	OldPlacements []OldPlacements `json:"oldPlacements,omitempty"`
	// Planned new placements (pending or moved) - note that moved will get a new uid
	NewPlacements []NewPlacements `json:"newPlacements,omitempty"`
	// Desired placements per workload / per node
	WorkloadPerNode WorkloadPerNode `json:"workloadPerNode,omitempty"`
}

// ===== Runtime indices for fast execution =====

type WorkloadPerNodeCnts map[string]map[string]*atomic.Int32 // workloadKey -> node -> remaining

type ActivePlanState struct {
	ID                  string
	PlanDoc             *StoredPlan
	WorkloadPerNodeCnts WorkloadPerNodeCnts
	PlacementByName     map[string]string // pod ns/name -> targetNode
	Ctx                 context.Context
	Cancel              context.CancelFunc
}

// ===== Flow / misc =====

type FlowResult struct {
	PlanID                        string
	TargetNode                    string
	BatchSize                     int
	Moves, Evicts                 int
	TotalPostPlan, TotalPrePlan   int
	SolverStatus                  string
	TotalDuration, SolverDuration time.Duration
}

// ===== PodSet =====

type PodSet struct {
	mu sync.RWMutex
	m  map[types.UID]PodKey
}

type PodKey struct {
	UID       types.UID
	Namespace string
	Name      string
}
