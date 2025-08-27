package mycrossnodepreemption

import (
	"time"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	V2 = 2 // klog verbosity level; set to 0 for extra verbose logging

	// ======= Optimality where/when settings =======
	OptimizeCadence          = OptimizeInBatches    // Choices: OptimizeForEvery, OptimizeInBatches
	OptimizeAt               = OptimizeAtPostFilter // Choices: OptimizeAtPostFilter, OptimizeAtPreEnqueue
	OptimizationInterval     = 60 * time.Second
	OptimizationInitialDelay = 5 * time.Second

	// ======= Solver settings =======
	SolverTimeout     = 55 * time.Second
	SolverMode        = SolverModeLexi // Choices: SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)
	SolverPath        = "/opt/solver/main.py"

	// ======= Plan settings =======
	PlanExecutionTTL       = 60 * time.Second // how long a plan may run before being terminated
	PlanConfigMapNamespace = "kube-system"    // match kube-scheduler namespace
	PlanConfigMapLabelKey  = "crossnode-plan"
)
