// plugin_config.go
package mypriorityoptimizer

import (
	"context"

	"k8s.io/klog/v2"
)

// buildPluginConfigSnapshot collects all the values we care about right now.
// To extend, just add fields to PluginConfigSnapshot and populate them here.
func buildPluginConfigSnapshot() PluginConfigSnapshot {
	return PluginConfigSnapshot{
		Timestamp: getTimestampNowUtc(),

		Name:    Name,
		Version: PluginVersion,

		SystemNamespace:                   SystemNamespace,
		CacheWarmupSettleDelay:            CacheWarmupSettleDelay.String(),
		PluginReadinessUsableNodeInterval: PluginReadinessUsableNodeInterval.String(),

		OptimizeMode:                   getModeCombinedAsString(),
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
// SystemNamespace using ConfigMapDoc.ensureJson.
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

	// ConfigMap client (namespaced interface)
	cms := pl.Client.CoreV1().ConfigMaps(SystemNamespace)

	if err := doc.ensureJson(ctx, cms, snap); err != nil {
		klog.ErrorS(err, "persistPluginConfig: failed to create/update plugin configuration ConfigMap",
			"namespace", doc.Namespace, "name", doc.Name)
		return err
	}

	klog.InfoS("persistPluginConfig: updated plugin configuration ConfigMap",
		"namespace", doc.Namespace, "name", doc.Name)
	return nil
}
