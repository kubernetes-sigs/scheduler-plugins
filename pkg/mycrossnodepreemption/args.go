// args.go

package mycrossnodepreemption

// ======= Optimality where/when settings =======

// OptimizeCadence is the frequency at which optimization is performed.
// Choices: "for_every", "in_batches", "continuously"
var OptimizeCadence = parseCadence(getenv("OPTIMIZE_CADENCE", "for_every"))

// OptimizeAt is the action point that triggers optimization.
// Choices: "preenqueue", "postfilter" (ignored in continuous mode)
var OptimizeAt = parseOptimizeAt(getenv("OPTIMIZE_AT", "postfilter"))

// OptimizationInterval is the duration between consecutive optimization runs.
// If a plan is actively being executed, the loop is skipped.
var OptimizationInterval = parseTime(getenv("OPTIMIZATION_INTERVAL", "30s"))

// OptimizationInitialDelay is the initial delay before the first optimization run.
var OptimizationInitialDelay = parseTime(getenv("OPTIMIZATION_INITIAL_DELAY", "15s"))

// ======= Solver settings =======

// SolverPythonTimeout is the timeout for the python solver to complete.
var SolverPythonTimeout = parseTime(getenv("SOLVER_PYTHON_TIMEOUT", "25s"))

// SolverFastEnabled indicates whether the fast solver is enabled.
var SolverFastEnabled = parseBool(getenv("SOLVER_FAST_ENABLED", "true"))

// SolverFastTimeout is the timeout for the fast solver to complete.
var SolverFastTimeout = parseTime(getenv("SOLVER_FAST_TIMEOUT", "500ms"))

// SolverPythonEnabled indicates whether the Python solver is enabled.
var SolverPythonEnabled = parseBool(getenv("SOLVER_PYTHON_ENABLED", "true"))

// ======= Plan settings =======

// PlanExecutionTimeout is the maximum duration a plan may run before being terminated.
var PlanExecutionTimeout = parseTime(getenv("PLAN_EXECUTION_TIMEOUT", "60s"))
