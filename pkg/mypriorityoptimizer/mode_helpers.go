// mode_helpers.go
package mypriorityoptimizer

import (
	"fmt"
)

// isPerPodMode is the optimizer cadence that optimizes for every new pod.
func isPerPodMode() bool { return OptimizeMode == ModePerPod }

// isManualBlockingMode is true if the mode is ManualBlocking.
func isManualBlockingMode() bool { return OptimizeMode == ModeManualBlocking }

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

// getModeAsString returns a human-readable representation of the OptimizeMode.
func getModeAsString() string {
	switch OptimizeMode {
	case ModePerPod:
		return "PerPod"
	case ModePeriodic:
		return "Periodic"
	case ModeInterlude:
		return "Interlude"
	case ModeManual:
		return "Manual"
	case ModeManualBlocking:
		return "ManualBlocking"
	default:
		return "Periodic"
	}
}

// getSyncAsString returns "Asynch" or "Synch" based on the OptimizeSolveSynch setting.
func getSyncAsString() string {
	if isAsyncSolving() {
		return "Asynch"
	}
	return "Synch"
}

// getModeCombinedAsString returns a string representation of the current strategy:
// "<Mode><Synch|Asynch>"
func getModeCombinedAsString() string {
	return fmt.Sprintf("%s/%s", getModeAsString(), getSyncAsString())
}
