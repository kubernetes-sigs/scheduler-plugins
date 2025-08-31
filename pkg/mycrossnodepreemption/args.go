// args.go

package mycrossnodepreemption

// ======= Optimality where/when settings =======
var OptimizeCadence = parseCadence(getenv("OPTIMIZE_CADENCE", "for_every")) // Choices: "for_every", "in_batches", "continuously"
var OptimizeAt = parseOptimizeAt(getenv("OPTIMIZE_AT", "postfilter"))       // Choices: "preenqueue", "postfilter" (ignored in continuous mode)
var OptimizationInterval = parseTime(getenv("OPTIMIZATION_INTERVAL", "30s"))
var OptimizationInitialDelay = parseTime(getenv("OPTIMIZATION_INITIAL_DELAY", "15s"))

// ======= Solver settings =======
var SolverTimeout = parseTime(getenv("SOLVER_TIMEOUT", "25s"))

// ======= Plan settings =======
var PlanExecutionTimeout = parseTime(getenv("PLAN_EXECUTION_TIMEOUT", "60s")) // how long a plan may run before being terminated
