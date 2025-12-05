// strategy_helpers.go

package mypriorityoptimizer

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// ==== Mode helpers =======================================================

// optimizeEvery is the optimizer cadence that optimizes for every new pod.
func optimizeEvery() bool { return OptimizeMode == ModeEvery }

// isGlobalMode is true for modes that operate on the accumulated pending set
// (as opposed to "every pod" modes).
func isGlobalMode() bool {
	switch OptimizeMode {
	case ModePeriodic, ModeManual, ModeInterlude:
		return true
	default:
		return false
	}
}

// optimizeAsync is true for modes where we:
//   - collect pods at PostFilter, and
//   - take Active only after we know a plan is worthwhile.
//
// "Every" is always treated as synchronous to keep semantics simple.
func optimizeAsync() bool {
	if OptimizeMode == ModeEvery {
		return false
	}
	return !OptimizeSolveSynch
}

// optimizeManual runs in manual global mode: pods are collected but the solver
// is only run on explicit HTTP /solve.
func optimizeManual() bool { return OptimizeMode == ModeManual }

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeHookStage == StagePreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeHookStage == StagePostFilter }

// atPreEnqueue returns true if the stage is PreEnqueue.
func (stage StageType) atPreEnqueue() bool { return stage == StagePreEnqueue }

// atPostFilter returns true if the stage is PostFilter.
func (stage StageType) atPostFilter() bool { return stage == StagePostFilter }

// strategyToString returns a string representation of the current strategy:
// "<Mode><Synch|Asynch>/<PreEnqueue|PostFilter>"
func strategyToString() string {
	var modeStr string
	switch OptimizeMode {
	case ModeEvery:
		modeStr = "Every"
	case ModePeriodic:
		modeStr = "Periodic"
	case ModeInterlude:
		modeStr = "Interlude"
	case ModeManual:
		modeStr = "Manual"
	default:
		modeStr = "Periodic"
	}

	syncStr := "Synch"
	if optimizeAsync() {
		syncStr = "Asynch"
	}

	stageStr := "PreEnqueue"
	if optimizeAtPostFilter() {
		stageStr = "PostFilter"
	}

	return fmt.Sprintf("%s%s/%s", modeStr, syncStr, stageStr)
}

// ==== Parsing ============================================================

// parseOptimizeMode parses a cadence string and returns the corresponding OptimizeModeType.
//
// Allowed values:
//   - "every"
//   - "periodic"
//   - "manual"
//   - "interlude"
func parseOptimizeMode(s string) OptimizeModeType {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "every":
		return ModeEvery
	case "periodic":
		return ModePeriodic
	case "manual":
		return ModeManual
	case "interlude":
		return ModeInterlude
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_MODE value; defaulting to 'periodic'", "value", s)
		return ModePeriodic
	}
}

// parseOptimizeHookStage parses an optimization "at" string and returns the StageType.
//
// For manual mode we always block at PreEnqueue, regardless of the env.
// For other async *global* modes we always collect pods at PostFilter.
func parseOptimizeHookStage(s string) StageType {
	// Manual mode: always gate at PreEnqueue so pods are blocked until /solve.
	if OptimizeMode == ModeManual {
		return StagePreEnqueue
	}

	// Async global (non-manual) modes → always PostFilter.
	if isGlobalMode() && optimizeAsync() {
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

// ==== Strategy decision ==================================================

func (pl *SharedState) decideStrategy(stage StageType) StrategyDecision {
	// If there's an active plan, block all new pods.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlock
	}

	// Global modes (Periodic, Manual, Interlude) accumulate pods,
	// and optimization is driven by the periodic/interlude loops or HTTP /solve.
	if isGlobalMode() &&
		((optimizeAtPreEnqueue() && stage.atPreEnqueue()) ||
			(optimizeAtPostFilter() && stage.atPostFilter())) {
		return DecideProcessLater
	}

	// ModeEvery: optimize for every new pod at the configured stage.
	if optimizeEvery() &&
		((optimizeAtPreEnqueue() && stage.atPreEnqueue()) ||
			(optimizeAtPostFilter() && stage.atPostFilter())) {
		return DecideProcess
	}

	// If not at the stage of optimization, allow all pods
	return DecidePass
}
