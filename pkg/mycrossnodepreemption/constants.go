// constants.go

package mycrossnodepreemption

import "time"

const (
	// ================ General settings =======================

	// Name is the name of the component.
	Name = "MyCrossNodePreemption"
	// Version is the current version of the plugin.
	Version = "v1.5.0"
	// MyVerbosity is the klog verbosity level; set to 0 for extra verbose logging.
	MyVerbosity = 2
	// CacheWarmupSettleDelay is the duration to wait before proceeding after cache has warmed up.
	CacheWarmupSettleDelay = 2 * time.Second
	// =========================================================

	// ================ Solver settings ========================

	// SolverPath is the path to the solver executable.
	// See Dockerfile for details.
	SolverPath      = "/opt/solver/main.py"
	SolverPythonBin = "/opt/venv/bin/python"
	// SolverLogProgress is a flag that enables/disables logging of solver progress.
	SolverLogProgress = false
	// =========================================================

	// ================ Plan settings ==========================

	// PlanConfigMapNamespace is the namespace of the ConfigMap used for plan configuration.
	// For ease of use, it should match the kube-scheduler namespace.
	PlanConfigMapNamespace = "kube-system"
	// PlanConfigMapLabelKey is the name of the ConfigMap used for plan configuration.
	PlanConfigMapLabelKey = "crossnode-plan"
	// PlanPendingBindInterval is the interval at which pending binds are retried.
	PlanPendingBindInterval = 250 * time.Millisecond
	// PlansToRetain is the number of ConfigMaps plans to retain before the oldest are deleted.
	PlansToRetain = 30
	// NudgeBlockedInterval is how often to try waking one blocked pod when idle in Every@PreEnqueue.
	// We need this functionality at this mode, as if we activate all blocked pods at once
	// over and over again in onPlanSettled, we end up with a large waiting time in the queue.
	NudgeBlockedInterval = 200 * time.Millisecond
	// =========================================================
)
