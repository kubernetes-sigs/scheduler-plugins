// solver_types.go
package mypriorityoptimizer

// SolverPhase represents a phase/stage of the solver process.
type SolverPhase struct {
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
