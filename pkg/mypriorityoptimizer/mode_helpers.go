// mode_helpers.go

package mypriorityoptimizer

import (
	"fmt"
)

// ==== Mode helpers =======================================================

// optimizePerPod is the optimizer cadence that optimizes for every new pod.
func optimizePerPod() bool { return OptimizeMode == ModePerPod }

// optimizeAsync is true for modes where we:
//   - collect pods at PostFilter, and
//   - take Active only after we know a plan is worthwhile.
// "PerPod" is always treated as synchronous.
func optimizeAsync() bool {
	if OptimizeMode == ModePerPod {
		return false
	}
	return !OptimizeSolveSynch
}

// isManualMode is true if the mode is Manual.
func isManualMode() bool { return OptimizeMode == ModeManual }

// isGlobalMode is true for modes that operate on the accumulated pending set
// (as opposed to per_pod modes).
func isGlobalMode() bool {
	switch OptimizeMode {
	case ModePeriodic, ModeInterlude, ModeManual:
		return true
	default:
		return false
	}
}

// hookAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func hookAtPreEnqueue() bool { return OptimizeHookStage == StagePreEnqueue }

// hookAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func hookAtPostFilter() bool { return OptimizeHookStage == StagePostFilter }

// atPreEnqueue returns true if the stage is PreEnqueue.
func (stage StageType) atPreEnqueue() bool { return stage == StagePreEnqueue }

// atPostFilter returns true if the stage is PostFilter.
func (stage StageType) atPostFilter() bool { return stage == StagePostFilter }

// modeToString returns a string representation of the current strategy:
// "<Mode><Synch|Asynch>/<PreEnqueue|PostFilter>"
func modeToString() string {
	var modeStr string
	switch OptimizeMode {
	case ModePerPod:
		modeStr = "PerPod"
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
	if hookAtPostFilter() {
		stageStr = "PostFilter"
	}

	return fmt.Sprintf("%s%s/%s", modeStr, syncStr, stageStr)
}
