// types.go
package mycrossnodepreemption

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type MyCrossNodePreemption struct {
	Handle     framework.Handle
	Client     kubernetes.Interface
	Active     atomic.Bool
	ActivePlan atomic.Pointer[ActivePlanState]
	Blocked    *PodSet
	Batched    *PodSet
}

var (
	ErrActiveInProgress = errors.New("active plan in progress")
	ErrSolver           = errors.New("solver failed")
	ErrRegisterPlan     = errors.New("failed to register plan")
)

const (
	// Deployment and CronJob are handled otherwise
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

type OptimizationCadenceMode int

const (
	OptimizeForEvery  OptimizationCadenceMode = iota // solve per unschedulable preemptor
	OptimizeInBatches                                // periodic cohort solving
)

type OptimizationAtMode int

const (
	OptimizeAtPreEnqueue OptimizationAtMode = iota // act in PreEnqueue
	OptimizeAtPostFilter                           // act in PostFilter
)

const (
	SolverModeLexi     = "lexi"
	SolverModeWeighted = "weighted"
)

type SolveMode int

const (
	SolveBatch SolveMode = iota
	SolveSingle
)

// decideStrategy says what to do with this pod at this phase.
type StrategyDecision int

const (
	DecidePassThrough StrategyDecision = iota // let default scheduler proceed
	DecideBatch                               // add to Batched and stop here
	DecideEvery                               // run the single-preemptor optimization flow now
	DecideBlockActive                         // plan active; block this pod
)

type Phase string

const (
	PhasePreEnqueue Phase = "PreEnqueue"
	PhasePostFilter Phase = "PostFilter"
	PhaseBatch      Phase = "BatchLoop"
)

type FlowResult struct {
	PlanID                         string
	Nominated                      string
	BatchSize                      int
	Moves, Evicts                  int
	NewScheduled, StillUnscheduled int
	SolverStatus                   string
	TotalDuration, SolverDuration  time.Duration
}

func (phase Phase) atPreEnqueue() bool { return phase == PhasePreEnqueue }
func (phase Phase) atPostFilter() bool { return phase == PhasePostFilter }

type ActivePlanState struct {
	ID        string
	PlanDoc   *StoredPlan
	Remaining WorkloadNodeCounters
	Ctx       context.Context
	Cancel    context.CancelFunc
}

type StoredPlan struct {
	PluginVersion    string                    `json:"pluginVersion"`
	Mode             string                    `json:"mode"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	PendingPod       string                    `json:"pendingPod,omitempty"`
	Completed        bool                      `json:"completed"`
	CompletedAt      *time.Time                `json:"completedAt,omitempty"`
	PendingUID       string                    `json:"pendingUID,omitempty"`
	TargetNode       string                    `json:"targetNode,omitempty"`
	SolverOutput     *SolverOutput             `json:"solverOutput,omitempty"`
	Plan             Plan                      `json:"plan"`
	PlacementsByName map[string]string         `json:"placementsByName,omitempty"`
	WkDesiredPerNode map[string]map[string]int `json:"wkDesiredPerNode,omitempty"`
}

type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode,omitempty"`
	Placements    map[string]string `json:"placements"`
	Evictions     []SolverEviction  `json:"evictions"`
}

type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type Plan struct {
	TargetNode string  `json:"targetNode"` // may be empty in batch mode
	Moves      []Move  `json:"moves"`
	Evicts     []Evict `json:"evicts"`
}

type Move struct {
	Pod      PodRef `json:"pod"`
	FromNode string `json:"fromNode"`
	ToNode   string `json:"toNode"`
	CPUm     int64  `json:"cpu_m"`
	MemBytes int64  `json:"mem_bytes"`
}

type Evict struct {
	Pod      PodRef `json:"pod"`
	FromNode string `json:"fromNode"`
	CPUm     int64  `json:"cpu_m"`
	MemBytes int64  `json:"mem_bytes"`
}

type PodRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type SolverInput struct {
	Preemptor      *SolverPod   `json:"preemptor,omitempty"` // nil => batch mode
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	LogProgress    bool         `json:"log_progress,omitempty"`
	Mode           string       `json:"solver_mode,omitempty"` // "lexi" or "weighted"
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

type WorkloadNodeCounters map[string]map[string]*atomic.Int32 // workloadKey -> node -> remaining

type WorkloadKind int

type WorkloadKey struct {
	Kind      WorkloadKind
	Namespace string
	Name      string
}

type PodSet struct {
	mu sync.RWMutex
	m  map[types.UID]PodKey
}

type PodKey struct {
	UID       types.UID
	Namespace string
	Name      string
}
