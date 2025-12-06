// mode_helpers.go
package mypriorityoptimizer

import (
	"fmt"
)

// ==== Mode helpers =======================================================

// isPerPodMode is the optimizer cadence that optimizes for every new pod.
func isPerPodMode() bool { return OptimizeMode == ModePerPod }

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

// isManualMode is true if the mode is Manual.
func isManualMode() bool { return OptimizeMode == ModeManual }

// isAsyncSolving is true for modes where we:
// - collect pods at PostFilter, and
// - take Active only after we know a plan is worthwhile.
// "PerPod" is always treated as synchronous.
func isAsyncSolving() bool {
	if OptimizeMode == ModePerPod {
		return false
	}
	return !OptimizeSolveSynch
}

// hookAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func hookAtPreEnqueue() bool {
	if OptimizeMode == ModePerPod {
		return false
	}
	return OptimizeHookStage == StagePreEnqueue
}

// hookAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func hookAtPostFilter() bool {
	if OptimizeMode == ModePerPod {
		return true
	}
	return OptimizeHookStage == StagePostFilter
}

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

	stageStr := "PostFilter"
	if hookAtPreEnqueue() && !isPerPodMode() {
		stageStr = "PreEnqueue"
	}

	syncStr := "Synch"
	if isAsyncSolving() {
		syncStr = "Asynch"
	}

	return fmt.Sprintf("%s/%s/%s", modeStr, stageStr, syncStr)
}
