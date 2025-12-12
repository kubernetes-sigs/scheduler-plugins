// args.go
package mypriorityoptimizer

// ======= Optimality where/when settings =======

// OptimizeMode is the frequency at which optimization is performed. Choices:
// "per_pod", "periodic", "interlude", "manual", "manual_blocking"
var OptimizeMode = parseOptimizeMode(getEnv("OPTIMIZE_MODE", "periodic"))

// OptimizeSolveSynch controls whether solver runs use the synchronous or
// asynchronous flow w.r.t. taking the Active lock.
//
// true = "synchronous" (take Active before planContext). false = "asynchronous"
// (take Active only when we know a plan is worth applying)
var OptimizeSolveSynch = parseBool(getEnv("OPTIMIZE_SOLVE_SYNCH", "true"))

// OptimizePeriodicInterval is the duration between consecutive optimization
// runs in periodic mode. If a plan is currently active, the loop is skipped.
var OptimizePeriodicInterval = parseTime(getEnv("OPTIMIZE_PERIODIC_INTERVAL", "30s"))

// OptimizeInterludeDelay is the duration of idle time (no changes in the
// pending set) before triggering interlude optimization.
var OptimizeInterludeDelay = parseTime(getEnv("OPTIMIZE_INTERLUDE_DELAY", "2s"))

// OptimizeInterludeCheckInterval is the interval at which we poll for interlude
// "free time" conditions.
var OptimizeInterludeCheckInterval = parseTime(getEnv("OPTIMIZE_INTERLUDE_CHECK_INTERVAL", "250ms"))

// Address the HTTP server should listen on (used for manual optimization and
// debugging in all modes). Only works on a KWOK cluster if running with binary
// runtime. Examples: ":18080", "0.0.0.0:18080"
var HTTPAddr = getEnv("HTTP_ADDR", ":18080")

// ======= Solver settings =======

// Save failed attempts to config map (for debugging)
var SolverSaveAllAttempts = parseBool(getEnv("SOLVER_SAVE_FAILED_ATTEMPTS", "true"))

// SolverPythonEnabled indicates whether the Python solver is enabled.
var SolverPythonEnabled = parseBool(getEnv("SOLVER_PYTHON_ENABLED", "false"))

// SolverPythonTimeout is the timeout for the python solver to complete.
var SolverPythonTimeout = parseTime(getEnv("SOLVER_PYTHON_TIMEOUT", "10s"))

// SolverPythonScriptPath is the path to the solver executable.
var SolverPythonScriptPath = getEnv("SOLVER_PATH", "/opt/solver/main.py")

// Path to the Python binary to use for running the solver.
var SolverPythonBin = getEnv("SOLVER_PYTHON_BIN", "/opt/venv/bin/python")

// SolverPythonGapLimit is the gap to optimality for the python solver (0.00 =
// optimal).
var SolverPythonGapLimit = parseFloat(getEnv("SOLVER_PYTHON_GAP_LIMIT", "0.00"), 0.00, 1.00)

// SolverPythonGuaranteedTierFraction is the guaranteed fraction of time for all
// tiers (0.00-1.00).
var SolverPythonGuaranteedTierFraction = parseFloat(getEnv("SOLVER_PYTHON_GUARANTEED_TIER_FRACTION", "0.40"), 0.00, 1.00)

// SolverPythonMoveFractionOfTier is the fraction of a tier's budget for moves
// (0.00-1.00).
var SolverPythonMoveFractionOfTier = parseFloat(getEnv("SOLVER_PYTHON_MOVE_FRACTION_OF_TIER", "0.30"), 0.00, 1.00)

// SolverPythonGraceMs is the grace period for the python solver (ms).
var SolverPythonGraceMs = parseInt(getEnv("SOLVER_PYTHON_GRACE_MS", "1000"))

// ======= Plan settings =======

// PlanExecutionTimeout is the maximum duration a plan may run before being
// terminated.
var PlanExecutionTimeout = parseTime(getEnv("PLAN_EXECUTION_TIMEOUT", "20s"))
