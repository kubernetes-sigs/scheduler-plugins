// args.go

package mypriorityoptimizer

// ======= Optimality where/when settings =======

// OptimizeMode is the frequency at which optimization is performed.
// Choices: "every", "all_synch", "all_asynch", "manual_all_synch", "freetime_synch", "freetime_asynch"
var OptimizeMode = parseOptimizeMode(getenv("OPTIMIZE_MODE", "all_synch"))

// OptimizeHookStage is the action point that triggers optimization.
// Choices: "preenqueue", "postfilter" (ignored in all_asynch mode)
var OptimizeHookStage = parseOptimizeHookStage(getenv("OPTIMIZE_HOOK_STAGE", "postfilter"))

// OptimizeInterval is the duration between consecutive optimization runs.
// If a plan is actively being executed, the loop is skipped.
var OptimizeInterval = parseTime(getenv("OPTIMIZE_INTERVAL", "30s"))

// OptimizeInitialDelay is the initial delay before the first optimization run.
var OptimizeInitialDelay = parseTime(getenv("OPTIMIZE_INITIAL_DELAY", "5s"))

// OptimizeFreeTimeDelay is the duration of idle time (no new pods arriving) before triggering freetime optimization.
var OptimizeFreeTimeDelay = parseTime(getenv("OPTIMIZE_FREE_TIME_DELAY", "2s"))

// OptimizeFreeTimeCheckInterval is the interval at which to check for free time conditions.
var OptimizeFreeTimeCheckInterval = parseTime(getenv("OPTIMIZE_FREE_TIME_CHECK_INTERVAL", "2s"))

// Address the HTTP server should listen on (only used in ModeManual, ModeAllSynch, ModeAllAsynch).
// Only works on a KWOK cluster if running with binary runtime.
// Examples: ":18080", "0.0.0.0:18080"
var HTTPAddr = getenv("HTTP_ADDR", ":18080")

// ======= Solver settings =======

// Save failed attempts to config map (for debugging)
var SolverSaveAllAttempts = parseBool(getenv("SOLVER_SAVE_FAILED_ATTEMPTS", "true"))

// Whether solvers should receive and honor improvement hints.
var SolverUseHints = parseBool(getenv("SOLVER_USE_HINTS", "false"))

// SolverPythonEnabled indicates whether the Python solver is enabled.
var SolverPythonEnabled = parseBool(getenv("SOLVER_PYTHON_ENABLED", "false"))

// SolverPythonTimeout is the timeout for the python solver to complete.
var SolverPythonTimeout = parseTime(getenv("SOLVER_PYTHON_TIMEOUT", "10s"))

// SolverPythonGapLimit is the gap to optimality for the python solver (0.00 = optimal).
var SolverPythonGapLimit = parseFloat(getenv("SOLVER_PYTHON_GAP_LIMIT", "0.00"), 0.00, 1.00)

// SolverPythonGuaranteedTierFraction is the guaranteed fraction of time for all tiers (0.00-1.00).
var SolverPythonGuaranteedTierFraction = parseFloat(getenv("SOLVER_PYTHON_GUARANTEED_TIER_FRACTION", "0.40"), 0.00, 1.00)

// SolverPythonMoveFractionOfTier is the fraction of a tier's budget for moves (0.00-1.00).
var SolverPythonMoveFractionOfTier = parseFloat(getenv("SOLVER_PYTHON_MOVE_FRACTION_OF_TIER", "0.30"), 0.00, 1.00)

// SolverPythonGraceMs is the grace period for the python solver (ms).
var SolverPythonGraceMs = parseInt(getenv("SOLVER_PYTHON_GRACE_MS", "1000"))

// SolverPythonNumLowerPriorities controls how many distinct priorities (from the lowest)
// are included in the optimization (0 = all priorities).
var SolverPythonNumLowerPriorities = parseInt(getenv("SOLVER_PYTHON_NUM_LOWER_PRIORITIES", "0"))

// ======= Plan settings =======

// PlanExecutionTimeout is the maximum duration a plan may run before being terminated.
var PlanExecutionTimeout = parseTime(getenv("PLAN_EXECUTION_TIMEOUT", "20s"))
