package mycrossnodepreemption

import (
	"time"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	V2 = 2 // klog verbosity level; set to 0 for extra verbose logging

	// ======= Optimality where/when settings =======
	OptimizeCadence          = OptimizeContinuously // Choices: OptimizeForEvery, OptimizeInBatches, OptimizeContinuously
	OptimizeAt               = OptimizeAtPostFilter // Choices: OptimizeAtPreEnqueue, OptimizeAtPostFilter (ignored in continuous mode)
	OptimizationInterval     = 30 * time.Second
	OptimizationInitialDelay = 15 * time.Second // 15 seconds, should be the minimum...

	// ======= Solver settings =======
	SolverTimeout     = 25 * time.Second
	SolverMode        = SolverModeLexi // Choices: SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)
	SolverPath        = "/opt/solver/main.py"

	// ======= Plan settings =======
	PlanExecutionTTL        = 60 * time.Second // how long a plan may run before being terminated
	PlanConfigMapNamespace  = "kube-system"    // match kube-scheduler namespace
	PlanConfigMapLabelKey   = "crossnode-plan"
	NudgeBlockedInterval    = 200 * time.Millisecond // How often to try waking one blocked pod when idle in ForEvery@PreEnqueue.
	PlanPendingBindTimeout  = 5 * time.Second
	PlanPendingBindInterval = 250 * time.Millisecond
)
