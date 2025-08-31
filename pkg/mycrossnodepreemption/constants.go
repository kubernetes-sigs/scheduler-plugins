// constants.go

package mycrossnodepreemption

import "time"

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"
	V2      = 2 // klog verbosity level; set to 0 for extra verbose logging

	// ======= Solver settings =======
	SolverPath        = "/opt/solver/main.py"
	SolverMode        = SolverModeLexi // Choices: SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)

	// ======= Plan settings =======
	PlanConfigMapNamespace  = "kube-system" // match kube-scheduler namespace
	PlanConfigMapLabelKey   = "crossnode-plan"
	PlanPendingBindInterval = 250 * time.Millisecond
	NudgeBlockedInterval    = 200 * time.Millisecond // How often to try waking one blocked pod when idle in ForEvery@PreEnqueue.
)
