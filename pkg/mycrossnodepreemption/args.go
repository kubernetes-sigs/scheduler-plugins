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

// SolverPythonEnabled indicates whether the Python solver is enabled.
var SolverPythonEnabled = parseBool(getenv("SOLVER_PYTHON_ENABLED", "false"))

// SolverPythonTimeout is the timeout for the python solver to complete.
var SolverPythonTimeout = parseTime(getenv("SOLVER_PYTHON_TIMEOUT", "25s"))

// SolverBfsEnabled indicates whether the BFS solver is enabled.
var SolverBfsEnabled = parseBool(getenv("SOLVER_BFS_ENABLED", "false"))

// SolverBfsTimeout is the timeout for the BFS solver to complete.
var SolverBfsTimeout = parseTime(getenv("SOLVER_BFS_TIMEOUT", "500ms"))

// SolverSwapEnabled indicates whether the swap solver is enabled.
var SolverSwapEnabled = parseBool(getenv("SOLVER_SWAP_ENABLED", "false"))

// SolverSwapTimeout is the timeout for the swap solver to complete.
var SolverSwapTimeout = parseTime(getenv("SOLVER_SWAP_TIMEOUT", "500ms"))

// ======= Plan settings =======

// PlanExecutionTimeout is the maximum duration a plan may run before being terminated.
var PlanExecutionTimeout = parseTime(getenv("PLAN_EXECUTION_TIMEOUT", "60s"))
