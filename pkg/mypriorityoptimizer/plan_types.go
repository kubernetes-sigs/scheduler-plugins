package mypriorityoptimizer

import (
	"time"
)

type Plan struct {
	// Evicted pods
	Evicts []SolverPod `json:"evicts"`
	// Moved pods
	Moves []SolverPod `json:"moves"`
	// All pods and their old placements
	OldPlacements []SolverPod `json:"old_placements"`
	// All pods and their new placements
	NewPlacements []SolverPod `json:"new_placements"`
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
	SolverResult SolverResult `json:"solver_result"`
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
