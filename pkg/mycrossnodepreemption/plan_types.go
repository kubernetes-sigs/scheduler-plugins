// plan_types.go

package mycrossnodepreemption

import "time"

type Plan struct {
	// Evicted pods
	Evicts []Placement `json:"evicts"`
	// Moved pods
	Moves []NewPlacement `json:"moves"`
	// All pods and their old placements
	OldPlacements []Placement `json:"oldPlacements"`
	// All pods and their new placements
	NewPlacements []NewPlacement `json:"newPlacements"`
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string `json:"placementByName"`
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotas WorkloadQuotas `json:"workloadQuotas"`
	// Nominated node for the preemptor pod (if any)
	NominatedNode string `json:"nominatedNode"`
}

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
	// Solver summary (status & score)
	Solver SolverResult `json:"solver"`
	// Single-preemptor metadata
	Preemptor *Preemptor `json:"preemptor,omitempty"`
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

// Preemptor represents a preemptor pod and its nominated node.
type Preemptor struct {
	// Pod being preempted
	Pod Pod `json:"pod"`
	// Nominated node for the preemptor pod
	NominatedNode string `json:"nominatedNode"`
}
