package mypriorityoptimizer

// PythonSolverOutput is the output from the Python solver, extending SolverOutput.
type PythonSolverOutput struct {
	// Embed the generic solver fields (status, placements, evictions, duration)
	SolverOutput
	// SolvePhases of the solver (Python-specific)
	SolvePhases []SolverPhase `json:"phases,omitempty"`
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
	SolverInput SolverInput `json:"solver_input"`
	// Python solver options
	SolverOptions PythonSolverOptions `json:"solver_options"`
}
