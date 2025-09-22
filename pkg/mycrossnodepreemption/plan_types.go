// plan_types.go

package mycrossnodepreemption

import "time"

type PlanBuild struct {
	// Evicted pods
	Evicts []Placement
	// Moved pods
	Moves []NewPlacement
	// All pods and their old placements
	OldPlacements []Placement
	// All pods and their new placements
	NewPlacements []NewPlacement
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotas WorkloadQuotas
	// Nominated node for the preemptor pod (if any)
	NominatedNode string
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
	// Status of the plan
	Status PlanStatus `json:"status"`
	// Single-preemptor metadata
	Preemptor *Preemptor `json:"preemptor,omitempty"`
	// Evicted pods
	Evicts []Placement `json:"evicts,omitempty"`
	// Moved pods
	Moves []NewPlacement `json:"moves,omitempty"`
	// Solver summary (status & score)
	Solver SolverResult `json:"solver"`
	// All pods and their old placements
	OldPlacements []Placement `json:"oldPlacements,omitempty"`
	// All pods and their new placements
	NewPlacement []NewPlacement `json:"newPlacement,omitempty"`
	// Placement by name for standalone pods: ns/name -> node
	PlacementByName map[string]string `json:"placementsByName,omitempty"`
	// Workload quotas for new placed pods that are part of a workload
	WorkloadQuotas WorkloadQuotas `json:"workloadQuotas,omitempty"`
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
