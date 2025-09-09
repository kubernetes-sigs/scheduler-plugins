// pkg/mycrossnodepreemption/types.go
package mycrossnodepreemption

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
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

// ===== Plan =====

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
	Preemptor *Preemtor `json:"preemptor,omitempty"`
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

type PlanStatus string

const (
	PlanStatusActive    PlanStatus = "Active"
	PlanStatusCompleted PlanStatus = "Completed"
	PlanStatusFailed    PlanStatus = "Failed"
)

// ===== Runtime indices for fast execution =====
type ActivePlanState struct {
	ID                  string
	WorkloadPerNodeCnts WorkloadPerNodeCnts
	PlacementByName     map[string]string // pod ns/name -> targetNode
	Ctx                 context.Context
	Cancel              context.CancelFunc
}

type WorkloadPerNodeCnts map[string]map[string]*atomic.Int32 // workloadKey -> node -> remaining

// ===== Workload types =====

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

type WorkloadQuotas map[string]map[string]int32

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

// ===== Solver types =====

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
	MaxTrials      int          `json:"max_trials,omitempty"` // for local search
}

type SolverOutput struct {
	Status     string         `json:"status"`
	Placements []NewPlacement `json:"placements"`
	Evictions  []Placement    `json:"evictions"`
}

type SolverPod struct {
	UID         string `json:"uid"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	ReqCPUm     int64  `json:"req_cpu_m"`
	ReqMemBytes int64  `json:"req_mem_bytes"`
	Priority    int32  `json:"priority"`
	Protected   bool   `json:"protected,omitempty"`
	// Current placement (pending if "")
	Node string `json:"node"`
}

type SolverNode struct {
	Name        string            `json:"name"`
	CapCPUm     int64             `json:"cap_cpu_m"`
	CapMemBytes int64             `json:"cap_mem_bytes"`
	Labels      map[string]string `json:"labels,omitempty"`
	// Runtime-only (not serialized)
	AllocCPUm     int64                 `json:"-"`
	AllocMemBytes int64                 `json:"-"`
	Pods          map[string]*SolverPod `json:"-"`
}

type SolverSummary struct {
	Name     string        `json:"name,omitempty"`
	Status   string        `json:"status,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
	Score    Score         `json:"score,omitempty"`
}

type SolverAttempt struct {
	Name    string
	Enabled bool
	Timeout time.Duration
	Trials  int
	FudgeMs int64
	Run     func(ctx context.Context, in SolverInput) (*SolverOutput, error)
}

// ===== Building blocks =====

type Score struct {
	PlacedByPriority map[string]int `json:"placed_by_priority,omitempty"`
	Evicted          int            `json:"evicted,omitempty"`
	Moved            int            `json:"moved,omitempty"`
}

type Pod struct {
	UID       string `json:"uid,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

type NewPlacement struct {
	Pod      Pod    `json:"pod"`
	FromNode string `json:"fromNode,omitempty"` // empty if new pod
	ToNode   string `json:"toNode"`
}

type Placement struct {
	Pod  Pod    `json:"pod"`
	Node string `json:"node"`
}

type Preemtor struct {
	Pod           Pod    `json:"pod"`
	NominatedNode string `json:"nominatedNode"`
}

type PreparedState struct {
	Nodes     map[string]*SolverNode
	Pods      map[string]*SolverPod
	Order     []*SolverNode
	Preemptor *SolverPod
	Worklist  []*SolverPod
	Single    bool
	MoveGate  *int32
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

// ToPodType converts a Pod to a PodType.
func ToPodType(p *v1.Pod, node string) SolverPod {
	return SolverPod{
		UID:         string(p.UID),
		Namespace:   p.Namespace,
		Name:        p.Name,
		ReqCPUm:     getPodCPURequest(p),
		ReqMemBytes: getPodMemoryRequest(p),
		Priority:    getPodPriority(p),
		Node:        node,
	}
}

// ===== PodSet =====

type PodSet struct {
	mu sync.RWMutex
	m  map[types.UID]PodKey
}

type PodKey struct {
	UID       types.UID // for fast lookup, we only use types.UID as key (not as a string)
	Namespace string
	Name      string
}
