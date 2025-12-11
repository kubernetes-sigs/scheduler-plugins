// solver_types.go
package mypriorityoptimizer

import (
	"context"
	"time"
)

// SolverInput is the input to a solver.
type SolverInput struct {
	// Preemptor pod (if any; single pod mode)
	Preemptor *Pod `json:"preemptor,omitempty"`
	// Nodes to consider
	Nodes []Node `json:"nodes"`
	// Pods to schedule / re-schedule
	Pods []Pod `json:"pods"`
	// Timeout for the solver (ms)
	TimeoutMs int64 `json:"timeout_ms"`
	// If true, ignore affinity rules
	IgnoreAffinity bool `json:"ignore_affinity"`
}

// SolverOutput is the output from a solver.
type SolverOutput struct {
	// Status of the solver (failed/infeasible/feasible/optimal)
	Status string `json:"status"`
	// Pod placements (new or moved)
	Placements []Pod `json:"placements"`
	// Evicted pods
	Evictions []Pod `json:"evictions"`
	// Duration in milliseconds of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// PythonSolverOutput is the output from the Python solver, extending SolverOutput.
type PythonSolverOutput struct {
	// Inline the generic solver fields (status, placements, evictions, duration)
	SolverOutput
	// Stages of the solver (Python-specific)
	Stages []SolverStage `json:"stages,omitempty"`
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
	// Status of the solver
	// filled from Output.Status when present
	Status string `json:"status,omitempty"`
	// DurationMs of the solver
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Score of the solution
	Score SolverScore `json:"score,omitempty"`
	// Stages of the solver
	Stages []SolverStage `json:"stages,omitempty"`
	// In-memory only (not exported)
	// Comparison vs previous leader (-1 worse, 0 tie, 1 better)
	CmpBase int `json:"-"`
	// Full detailed solver output (not exported)
	Output *SolverOutput `json:"-"`
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

type SolverStage struct {
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

// ExportedSolverStats is the structure used to export solver run statistics.
type ExportedSolverStats struct {
	// TimestampNs is the timestamp of the run in nanoseconds.
	TimestampNs int64 `json:"timestamp_ns"`
	// Error (if any)
	Error string `json:"error,omitempty"`
	// Best solver name
	BestName string `json:"best_name,omitempty"`
	// Baseline score
	Baseline *SolverScore `json:"baseline,omitempty"`
	// Best score
	Attempts []SolverResult `json:"attempts,omitempty"`
}

// PythonSolverOptions holds tuning knobs for the external Python solver.
type PythonSolverOptions struct {
	// Log solver progress
	LogProgress bool `json:"log_progress,omitempty"`
	// Gap to optimality (0.0 = ignore)
	GapLimit float64 `json:"gap_limit,omitempty"`
	// Guaranteed fraction of time for all tiers (0.0-1.0)
	GuaranteedTierFraction float64 `json:"guaranteed_tier_fraction,omitempty"`
	// Fraction of a tier's budget for moves (0.0-1.0)
	MoveFractionOfTier float64 `json:"move_fraction_of_tier,omitempty"`
}

// PythonSolverPayload is the full payload sent to the external Python solver.
type PythonSolverPayload struct {
	// Embedded solver input and options
	SolverInput `json:"solver_input"`
	// Python solver options
	PythonSolverOptions `json:"python_solver_options"`
}
