// args.go

package mycrossnodepreemption

// ======= Optimality where/when settings =======

// OptimizeCadence is the frequency at which optimization is performed.
// Choices: "for_every", "in_batches", "continuously"
var OptimizeCadence = parseCadence(getenv("OPTIMIZE_CADENCE", "in_batches"))

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
var SolverPythonTimeout = parseTime(getenv("SOLVER_PYTHON_TIMEOUT", "10s"))

// SolverBfsEnabled indicates whether the BFS solver is enabled.
var SolverBfsEnabled = parseBool(getenv("SOLVER_BFS_ENABLED", "false"))

// SolverBfsTimeout is the timeout for the BFS solver to complete.
var SolverBfsTimeout = parseTime(getenv("SOLVER_BFS_TIMEOUT", "500ms"))

// SolverLocalSearchEnabled indicates whether the swap solver is enabled.
var SolverLocalSearchEnabled = parseBool(getenv("SOLVER_LOCAL_SEARCH_ENABLED", "false"))

// SolverLocalSearchTimeout is the timeout for the swap solver to complete.
var SolverLocalSearchTimeout = parseTime(getenv("SOLVER_LOCAL_SEARCH_TIMEOUT", "500ms"))

// ======= Plan settings =======

// PlanExecutionTimeout is the maximum duration a plan may run before being terminated.
var PlanExecutionTimeout = parseTime(getenv("PLAN_EXECUTION_TIMEOUT", "20s"))

// ======= BFS solver settings =======

// Maximum search depth (i.e. number of moves allowed to place the preemptor)
var SolverBfsMaxDepth = parseInt(getenv("SOLVER_BFS_MAX_DEPTH", "5"))

// Maximum number of victims to consider per node.
// -1 = unlimited; 0 = no victims ⇒ search stops at that node.
var SolverBfsMaxVictimsPerNode = parseInt(getenv("SOLVER_BFS_MAX_VICTIMS_PER_NODE", "-1"))

// Maximum number of candidate destination nodes to consider per search level.
// -1 = unlimited; 0 = no destinations ⇒ search stops at that level.
var SolverBfsMaxDestsPerLevel = parseInt(getenv("SOLVER_BFS_MAX_DESTS_PER_LEVEL", "-1"))

// Max new states we’re allowed to emit while expanding *one* state.
// -1 = unlimited; 0 = emit no successors (useful for testing).
var SolverBfsMaxSuccessorsPerState = parseInt(getenv("SOLVER_BFS_MAX_SUCCESSORS_PER_STATE", "-1"))

// Max number of states we keep in the frontier after finishing a depth.
// If we created more, we keep the "best" ones by a greedy score
// (lowest remaining total deficit, then fewer deficit nodes, then signature).
// -1 = unlimited; 0 = frontier cleared ⇒ search stops at that depth.
var SolverBfsMaxFrontierPerDepth = parseInt(getenv("SOLVER_BFS_MAX_FRONTIER_PER_DEPTH", "-1"))

// ======= Local Search solver settings =======

// Maximum number of victims to consider per node.
// -1 = unlimited; 0 = no victims ⇒ search stops at that node.
var SolverLocalSearchMaxVictimsPerNode = parseInt(getenv("SOLVER_LOCAL_SEARCH_MAX_VICTIMS_PER_NODE", "8"))

// Number of independent restarts (fresh randomized plans) per target node.
// -1 = unlimited; 0 = no restarts (i.e., skip local-search altogether).
var SolverLocalSearchMaxRestartsPerTarget = parseInt(getenv("SOLVER_LOCAL_SEARCH_MAX_RESTARTS_PER_TARGET", "30"))

// Cap how many victim *probes* we try on the active target in a single attempt.
// -1 = unlimited; 0 = no probes (skip local search on this target).
var SolverLocalSearchMaxVictimProbesPerTarget = parseInt(getenv("SOLVER_LOCAL_SEARCH_MAX_VICTIM_PROBES_PER_TARGET", "50"))

// Maximum number of moves for the complete plan.
// -1 = unlimited; 0 = no moves ⇒ search stops immediately.
var SolverLocalSearchMaxMovesPerPlan = parseInt(getenv("SOLVER_LOCAL_SEARCH_MAX_MOVES_PER_PLAN", "5"))
