// mode_helpers.go
package mypriorityoptimizer

import (
	"fmt"
)

// isPerPodMode is the optimizer cadence that optimizes for every new pod.
func isPerPodMode() bool { return OptimizeMode == ModePerPod }

// isBackgroundMode is true for modes that operate on the accumulated pending set
// (as opposed to per_pod modes).
func isBackgroundMode() bool {
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

// modeToString returns a human-readable representation of the OptimizeMode.
func modeToString() string {
	switch OptimizeMode {
	case ModePerPod:
		return "PerPod"
	case ModePeriodic:
		return "Periodic"
	case ModeInterlude:
		return "Interlude"
	case ModeManual:
		return "Manual"
	default:
		return "Periodic"
	}
}

// stageToString returns a human-readable representation of a StageType.
func stageToString() string {
	stageStr := "PostFilter"
	if hookAtPreEnqueue() && !isPerPodMode() {
		stageStr = "PreEnqueue"
	}
	return stageStr
}

// syncToString returns "Asynch" or "Synch" based on the OptimizeSolveSynch setting.
func syncToString() string {
	if isAsyncSolving() {
		return "Asynch"
	}
	return "Synch"
}

// combinedModeToString returns a string representation of the current strategy:
// "<Mode><Synch|Asynch>/<PreEnqueue|PostFilter>"
func combinedModeToString() string {
	return fmt.Sprintf("%s/%s/%s", modeToString(), stageToString(), syncToString())
}
