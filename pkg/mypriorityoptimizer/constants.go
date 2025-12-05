// constants.go

package mypriorityoptimizer

import "time"

const (
	// ================ General settings =======================

	// Name is the name of the component.
	Name = "MyPriorityOptimizer"
	// Version is the current version of the plugin.
	Version = "v0.0.1"
	// MyV is the klog verbosity level; set to 0 for extra verbose logging.
	// Cannot be added to args.go as it needs to be a constant for build tags.
	MyV = 2
	// SystemNamespace is the namespace in which the plugin operates.
	// Used to prevent deletion of configmaps when cleaning up pods for a new run.
	// For ease of use, it should match the kube-scheduler namespace.
	SystemNamespace = "kube-system"
	// CacheWarmupSettleDelay is the duration to wait before proceeding after cache has warmed up.
	CacheWarmupSettleDelay = 2 * time.Second
	// PluginReadinessUsableNodeInterval is the interval at which the plugin checks for readiness.
	PluginReadinessUsableNodeInterval = 200 * time.Millisecond
	// =========================================================

	// ================ Solver settings ========================

	// SolverLogProgress is a flag that enables/disables logging of solver progress.
	SolverLogProgress = false
	// SolverConfigMapExportedStatsName is the name of the exported stats config map.
	SolverConfigMapExportedStatsName = "solver-stats"
	// SolverConfigMapLabelKey is the label key used for solver configuration config maps.
	SolverConfigMapLabelKey = "runs"
	// =========================================================

	// ================ Plan settings ==========================

	// PlanConfigMapLabelKey is the name of the ConfigMap used for plan configuration.
	PlanConfigMapLabelKey = "plan"
	// PlanCompletionCheckInterval is how often we check whether an active plan has reached its desired state.
	PlanCompletionCheckInterval = 250 * time.Millisecond
	// PlanPendingBindInterval is the interval at which pending binds are retried.
	PlanPendingBindInterval = 250 * time.Millisecond
	// PlansToRetain is the number of ConfigMaps plans to retain before the oldest are deleted.
	PlansToRetain = 32
	// NudgeBlockedInterval is how often to try waking one blocked pod when idle in PerPod@PreEnqueue.
	// We need this functionality at this mode, as if we activate all blocked pods at once
	// over and over again in onPlanCompleted, we end up with a large waiting time in the queue.
	NudgeBlockedInterval = 250 * time.Millisecond
	// The overall timeout for plan execution.
	PlanOverallTimeout = 10 * time.Second
	// Timeout for individual evict operations.
	EvictTimeout = 10 * time.Second
	// Timeout for individual recreate operations.
	RecreateTimeout = 10 * time.Second
	// Timeout for waiting for pods to be gone after eviction.
	WaitPodsGoneTimeout = 10 * time.Second
	// Interval for waiting for pods to be gone after eviction.
	WaitPodsGoneInterval = 250 * time.Millisecond
	// Degree of parallelism for eviction operations.
	EvictParallelism = 8
	// Degree of parallelism for pod recreation operations.
	RecreatePodParallelism = 8
	// =========================================================
)
