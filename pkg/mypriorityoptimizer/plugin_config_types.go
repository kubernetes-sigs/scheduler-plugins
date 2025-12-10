// plugin_config.go
package mypriorityoptimizer

import (
	"time"
)

// PluginConfigSnapshot captures a single, human-readable snapshot of the
// plugin's configuration (constants + env/args). Add new fields here as needed.
type PluginConfigSnapshot struct {
	Timestamp time.Time `json:"timestamp"`

	// General
	Name    string `json:"name"`
	Version string `json:"version"`

	SystemNamespace                   string `json:"systemNamespace"`
	CacheWarmupSettleDelay            string `json:"cacheWarmupSettleDelay"`
	PluginReadinessUsableNodeInterval string `json:"pluginReadinessUsableNodeInterval"`

	// Optimization settings (from args.go)
	OptimizeMode                   string `json:"optimizeMode"`
	OptimizePeriodicInterval       string `json:"optimizePeriodicInterval"`
	OptimizeInterludeDelay         string `json:"optimizeInterludeDelay"`
	OptimizeInterludeCheckInterval string `json:"optimizeInterludeCheckInterval"`

	// HTTP / control
	HTTPAddr string `json:"httpAddr"`

	// Solver settings
	SolverSaveAllAttempts              bool    `json:"solverSaveAllAttempts"`
	SolverPythonEnabled                bool    `json:"solverPythonEnabled"`
	SolverPythonTimeout                string  `json:"solverPythonTimeout"`
	SolverPythonScriptPath             string  `json:"solverPythonScriptPath"`
	SolverPythonBin                    string  `json:"solverPythonBin"`
	SolverPythonGapLimit               float64 `json:"solverPythonGapLimit"`
	SolverPythonGuaranteedTierFraction float64 `json:"solverPythonGuaranteedTierFraction"`
	SolverPythonMoveFractionOfTier     float64 `json:"solverPythonMoveFractionOfTier"`
	SolverPythonGraceMs                int     `json:"solverPythonGraceMs"`

	SolverLogProgress            bool   `json:"solverLogProgress"`
	SolverStatsConfigMapName     string `json:"solverStatsConfigMapName"`
	SolverStatsConfigMapLabelKey string `json:"solverStatsConfigMapLabelKey"`

	// Plan settings
	PlanExecutionTimeout        string `json:"planExecutionTimeout"`
	PlanConfigMapNamePrefix     string `json:"planConfigMapNamePrefix"`
	PlanConfigMapLabelKey       string `json:"planConfigMapLabelKey"`
	PlansToRetain               int    `json:"plansToRetain"`
	PlanCompletionCheckInterval string `json:"planCompletionCheckInterval"`
	PlanPendingBindInterval     string `json:"planPendingBindInterval"`
	PlanOverallTimeout          string `json:"planOverallTimeout"`
	NudgeBlockedInterval        string `json:"nudgeBlockedInterval"`
	EvictTimeout                string `json:"evictTimeout"`
	RecreateTimeout             string `json:"recreateTimeout"`
	WaitPodsGoneTimeout         string `json:"waitPodsGoneTimeout"`
	WaitPodsGoneInterval        string `json:"waitPodsGoneInterval"`
	EvictParallelism            int    `json:"evictParallelism"`
	RecreatePodParallelism      int    `json:"recreatePodParallelism"`
}
