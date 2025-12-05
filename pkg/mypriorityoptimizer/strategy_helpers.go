// strategy_helpers.go

package mypriorityoptimizer

func (pl *SharedState) decideStrategy(stage StageType) StrategyDecision {
	// If there's an active plan, block all new pods and let the plan's rules
	// (isPodAllowedByPlan/filterNodes) decide which ones may proceed.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlock
	}

	// Manual mode: global, but never auto-solves.
	//
	// - Manual + PreEnqueue:
	//     - If OptimizeHookStage == PreEnqueue, we "gate" here:
	//       pods are kept pending until /solve is called.
	//     - If OptimizeHookStage == PostFilter, PreEnqueue is pass-through.
	//
	// - Manual + PostFilter:
	//     - We *always* let normal scheduling proceed (DecidePass), i.e. we do
	//       not mark pods as "pending for optimizer" here, and we never call
	//       runFlow from PostFilter in manual mode.
	if isManualMode() {
		if hookAtPreEnqueue() && stage.atPreEnqueue() {
			// Gate at PreEnqueue: block until /solve
			return DecideProcessLater
		}
		// For PostFilter (and any other stage), don't interfere.
		return DecidePass
	}

	// Global modes (Periodic, Interlude) accumulate pods,
	// and optimization is driven by the periodic/interlude loops.
	if isGlobalMode() &&
		((hookAtPreEnqueue() && stage.atPreEnqueue()) ||
			(hookAtPostFilter() && stage.atPostFilter())) {
		return DecideProcessLater
	}

	// ModeEvery: optimize for every new pod at the configured stage.
	if optimizePerPod() &&
		((hookAtPreEnqueue() && stage.atPreEnqueue()) ||
			(hookAtPostFilter() && stage.atPostFilter())) {
		return DecideProcess
	}

	// If not at the stage of optimization, allow all pods
	return DecidePass
}
