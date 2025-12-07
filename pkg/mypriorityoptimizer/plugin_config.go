// plugin_config.go
package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/klog/v2"
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
	OptimizeHookStage              string `json:"optimizeHookStage"`
	OptimizeSolveSynch             bool   `json:"optimizeSolveSynch"`
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

// buildPluginConfigSnapshot collects all the values we care about right now.
// To extend, just add fields to PluginConfigSnapshot and populate them here.
func buildPluginConfigSnapshot() PluginConfigSnapshot {
	return PluginConfigSnapshot{
		Timestamp: time.Now().UTC(),

		Name:    Name,
		Version: Version,

		SystemNamespace:                   SystemNamespace,
		CacheWarmupSettleDelay:            CacheWarmupSettleDelay.String(),
		PluginReadinessUsableNodeInterval: PluginReadinessUsableNodeInterval.String(),

		OptimizeMode:                   combinedModeToString(),
		OptimizePeriodicInterval:       OptimizePeriodicInterval.String(),
		OptimizeInterludeDelay:         OptimizeInterludeDelay.String(),
		OptimizeInterludeCheckInterval: OptimizeInterludeCheckInterval.String(),

		HTTPAddr: HTTPAddr,

		SolverSaveAllAttempts:              SolverSaveAllAttempts,
		SolverPythonEnabled:                SolverPythonEnabled,
		SolverPythonTimeout:                SolverPythonTimeout.String(),
		SolverPythonScriptPath:             SolverPythonScriptPath,
		SolverPythonBin:                    SolverPythonBin,
		SolverPythonGapLimit:               SolverPythonGapLimit,
		SolverPythonGuaranteedTierFraction: SolverPythonGuaranteedTierFraction,
		SolverPythonMoveFractionOfTier:     SolverPythonMoveFractionOfTier,
		SolverPythonGraceMs:                SolverPythonGraceMs,

		SolverLogProgress:            SolverLogProgress,
		SolverStatsConfigMapName:     SolverStatsConfigMapName,
		SolverStatsConfigMapLabelKey: SolverStatsConfigMapLabelKey,

		PlanExecutionTimeout:        PlanExecutionTimeout.String(),
		PlanConfigMapLabelKey:       PlanConfigMapLabelKey,
		PlanConfigMapNamePrefix:     PlanConfigMapNamePrefix,
		PlansToRetain:               PlansToRetain,
		PlanCompletionCheckInterval: PlanCompletionCheckInterval.String(),
		PlanPendingBindInterval:     PlanPendingBindInterval.String(),
		PlanOverallTimeout:          PlanOverallTimeout.String(),
		NudgeBlockedInterval:        NudgeBlockedInterval.String(),
		EvictTimeout:                EvictTimeout.String(),
		RecreateTimeout:             RecreateTimeout.String(),
		WaitPodsGoneTimeout:         WaitPodsGoneTimeout.String(),
		WaitPodsGoneInterval:        WaitPodsGoneInterval.String(),
		EvictParallelism:            EvictParallelism,
		RecreatePodParallelism:      RecreatePodParallelism,
	}
}

// persistPluginConfig writes the PluginConfigSnapshot to a ConfigMap in
// SystemNamespace using ConfigMapDoc.ensureJson. It is best-effort and
// safe to call multiple times.
func (pl *SharedState) persistPluginConfig(ctx context.Context) error {
	if pl == nil || pl.Client == nil {
		// In tests we often have a nil Client; treat as no-op.
		return nil
	}

	doc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      PluginCfgConfigMapName,
		LabelKey:  PluginCfgConfigMapLabelKey,
		DataKey:   PluginCfgConfigMapLabelKey + ".json",
	}

	snap := buildPluginConfigSnapshot()
	if err := doc.ensureJson(ctx, pl.Client.CoreV1(), snap); err != nil {
		klog.ErrorS(err, "persistPluginConfig: failed to create/update plugin configuration ConfigMap",
			"namespace", doc.Namespace, "name", doc.Name)
		return err
	}

	klog.InfoS("persistPluginConfig: updated plugin configuration ConfigMap",
		"namespace", doc.Namespace, "name", doc.Name)
	return nil
}
