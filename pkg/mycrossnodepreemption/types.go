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

type MyCrossNodePreemption struct {
	Handle    framework.Handle
	Client    kubernetes.Interface
	ActivePtr atomic.Pointer[ActivePlanState]
	Blocked   *podSet
	Batched   *podSet
}

const (
	// Deployment and CronJob are handled otherwise
	// Deployment -> ReplicaSet
	// CronJob -> Job
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

type BatchIngressMode int

const (
	BatchOff BatchIngressMode = iota
	BatchPreEnqueue
	BatchPostFilter
)

const (
	SolverModeLexi     = "lexi"
	SolverModeWeighted = "weighted"
)

type ActivePlanState struct {
	ID        string               // configmap name (or any unique id)
	PlanDoc   *StoredPlan          // the same JSON you store in the ConfigMap
	Remaining WorkloadNodeCounters // workloadKey -> node -> *atomic.Int32
	Ctx       context.Context
	Cancel    context.CancelFunc
}

type WorkloadKind int

type WorkloadKey struct {
	Kind      WorkloadKind
	Namespace string
	Name      string
}

type podKey struct {
	UID       types.UID
	Namespace string
	Name      string
}

type podSet struct {
	mu sync.RWMutex
	m  map[types.UID]podKey
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

type StoredPlan struct {
	Completed        bool                      `json:"completed"`
	CompletedAt      *time.Time                `json:"completedAt,omitempty"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	PluginVersion    string                    `json:"pluginVersion"`
	PendingPod       string                    `json:"pendingPod"`
	PendingUID       string                    `json:"pendingUID"`
	TargetNode       string                    `json:"targetNode"`
	SolverOutput     *SolverOutput             `json:"solverOutput,omitempty"`
	Plan             Plan                      `json:"plan"`
	PlacementsByName map[string]string         `json:"placementsByName,omitempty"`
	WkDesiredPerNode map[string]map[string]int `json:"wkDesiredPerNode,omitempty"`
}
type SolverNode struct {
	Name     string            `json:"name"`
	CPUm     int64             `json:"cpu_m"`
	MemBytes int64             `json:"mem_bytes"`
	Labels   map[string]string `json:"labels,omitempty"`
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

type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type SolverInput struct {
	Preemptor      *SolverPod   `json:"preemptor,omitempty"` // nil => batch mode
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Mode           string       `json:"solver_mode,omitempty"` // "lexi" or "weighted"
	UseHints       bool         `json:"use_hints,omitempty"`
	Workers        int          `json:"workers,omitempty"`
}

type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode,omitempty"`
	Placements    map[string]string `json:"placements"`
	Evictions     []SolverEviction  `json:"evictions"`
}

type WorkloadNodeCounters map[string]map[string]*atomic.Int32 // workloadKey -> node -> remaining
