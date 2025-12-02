// strategy_helpers.go

package mypriorityoptimizer

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// optimizeEvery is the optimizer cadence that optimizes for every new pod.
func optimizeEvery() bool { return OptimizeMode == ModeEvery }

// optimizeAllSynch is the optimizer cadence that optimizes all pods and blocks while optimizing.
func optimizeAllSynch() bool { return OptimizeMode == ModeAllSynch }

// optimizeAllAsynch is the optimizer cadence that tries to optimize continuous if the cluster state hasn't drift too much during solver optimization.
func optimizeAllAsynch() bool { return OptimizeMode == ModeAllAsynch }

// optimizeManual collects like AllSynch but only optimizes when HTTP endpoint is called.
func optimizeManualAllSynch() bool { return OptimizeMode == ModeManualAllSynch }

// optimizeFreeTimeSynch optimizes all pods, but only when the queue has been
// quiescent for OptimizeFreeTimeDelay.
func optimizeFreeTimeSynch() bool { return OptimizeMode == ModeFreeTimeSynch }

// optimizeFreeTimeAsynch optimizes all pods in the background when the queue has
// been quiescent for OptimizeFreeTimeDelay.
func optimizeFreeTimeAsynch() bool { return OptimizeMode == ModeFreeTimeAsynch }

// optimizeAsync is true for all "async" modes where we:
//   - collect pods at PostFilter, and
//   - take Active later (after we know a plan is worthwhile).
func optimizeAsync() bool {
	return OptimizeMode == ModeAllAsynch || OptimizeMode == ModeFreeTimeAsynch
}

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeHookStage == StagePreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeHookStage == StagePostFilter }

// atPreEnqueue returns true if the stage is PreEnqueue.
func (stage StageType) atPreEnqueue() bool { return stage == StagePreEnqueue }

// atPostFilter returns true if the stage is PostFilter.
func (stage StageType) atPostFilter() bool { return stage == StagePostFilter }

// strategyToString returns a string representation of the optimization mode.
func strategyToString() string {
	var a string
	switch OptimizeMode {
	case ModeEvery:
		a = "Every"
	case ModeAllSynch:
		a = "AllSynch"
	case ModeAllAsynch:
		return "AllAsynch" // At is ignored
	case ModeManualAllSynch:
		a = "ManualAllSynch"
	case ModeFreeTimeSynch:
		a = "FreeTimeSynch"
	case ModeFreeTimeAsynch:
		return "FreeTimeAsynch" // At is ignored
	default:
		a = "AllSynch"
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// parseOptimizeMode parses a cadence string and returns the corresponding OptimizationCadenceMode.
func parseOptimizeMode(s string) OptimizeModeType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "every":
		return ModeEvery
	case "all_synch":
		return ModeAllSynch
	case "all_asynch":
		return ModeAllAsynch
	case "manual_all_synch":
		return ModeManualAllSynch
	case "freetime_synch":
		return ModeFreeTimeSynch
	case "freetime_asynch":
		return ModeFreeTimeAsynch
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_MODE value; defaulting to 'all_synch'", "value", s)
		return ModeAllSynch
	}
}

// parseOptimizeHookStage parses an optimization "at" string and returns the corresponding OptimizationAtMode.
func parseOptimizeHookStage(s string) StageType {
	// In async modes, we always collect pods at PostFilter and ignore the env.
	if optimizeAsync() {
		return StagePostFilter
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "preenqueue":
		return StagePreEnqueue
	case "postfilter":
		return StagePostFilter
	default:
		return StageNone
	}
}

// decideStrategy determines the strategy at the given stage.
func (pl *SharedState) decideStrategy(stage StageType) StrategyDecision {
	// If there's an active plan, block all new pods.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlock
	}

	// Modes: AllSynch/ManualAllSynch/FreeTimeSynch @ PreEnqueue/PostFilter
	// -> always accumulate pods (keep them pending), actual optimization is
	//    driven by periodicLoop (All*) or freetimeLoop (FreeTime*).
	if (optimizeAllSynch() || optimizeManualAllSynch() || optimizeFreeTimeSynch()) &&
		((optimizeAtPreEnqueue() && stage.atPreEnqueue()) ||
			(optimizeAtPostFilter() && stage.atPostFilter())) {
		return DecideProcessLater
	}

	// Modes: AllAsynch or FreeTimeAsynch @ PostFilter
	// -> always accumulate pods in PostFilter; async loops trigger optimization.
	if optimizeAsync() && stage.atPostFilter() {
		return DecideProcessLater
	}

	// Modes: Every@PreEnqueue or Every@PostFilter - optimize for every new pod
	if optimizeEvery() &&
		((optimizeAtPreEnqueue() && stage.atPreEnqueue()) ||
			(optimizeAtPostFilter() && stage.atPostFilter())) {
		return DecideProcess // optimize for every new pod
	}

	// If not at the stage of optimization, allow all pods
	return DecidePass
}
