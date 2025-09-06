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

// ======= BFS solver settings =======

// Maximum search depth (i.e. number of moves allowed to place the preemptor)
var SolverBfsMaxDepth = parseInt(getenv("SOLVER_BFS_MAX_DEPTH", "5"))

// Maximum number of victims to consider per node
var SolverBfsMaxVictimsPerNode = parseInt(getenv("SOLVER_BFS_MAX_VICTIMS_PER_NODE", "0"))

// Maximum number of candidate destination nodes to consider per search level
var SolverBfsMaxDestsPerLevel = parseInt(getenv("SOLVER_BFS_MAX_DESTS_PER_LEVEL", "0"))

// ======= Swap solver settings =======

// Maximum number of victims to consider per node
var SolverSwapMaxVictimsPerNode = parseInt(getenv("SOLVER_SWAP_MAX_VICTIMS_PER_NODE", "8"))

// Maximum number of relocation trials per node
var SolverSwapMaxTrialsPerNode = parseInt(getenv("SOLVER_SWAP_MAX_TRIALS_PER_NODE", "30"))

// Maximum number of moves for the complete plan
var SolverSwapMaxMovesForPendingPod = parseInt(getenv("SOLVER_SWAP_MAX_MOVES_FOR_PENDING_POD", "5"))

// Maximum number of relocation trials for the complete plan
var SolverSwapMaxTrialsPerPendingPod = parseInt(getenv("SOLVER_SWAP_MAX_TRIALS_PER_PENDING_POD", "100"))
