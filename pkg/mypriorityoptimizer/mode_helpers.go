// mode_helpers.go
package mypriorityoptimizer

// -------------------------
// isPerPodMode
// -------------------------

// isPerPodMode is the optimizer cadence that optimizes for every new pod.
// CHECKED
func isPerPodMode() bool { return OptimizeMode == ModePerPod }

// -------------------------
// isManualBlockingMode
// -------------------------

// isManualBlockingMode is true if the mode is ManualBlocking.
// CHECKED
func isManualBlockingMode() bool { return OptimizeMode == ModeManualBlocking }

// -------------------------
// isAsyncSolving
// -------------------------

// isAsyncSolving is true for modes where we:
// - collect pods at PostFilter, and
// - take Active only after we know a plan is worthwhile.
// PerPod is always treated as synchronous.
// CHECKED
func isAsyncSolving() bool {
	return OptimizeMode != ModePerPod && !OptimizeSolveSynch
}

// -------------------------
// getSyncAsString
// -------------------------

// getSyncAsString returns "Synch" or "Asynch".
// CHECKED
func getSyncAsString() string {
	if isAsyncSolving() {
		return "Asynch"
	}
	return "Synch"
}

// -------------------------
// getModeCombinedAsString
// -------------------------

// getModeCombinedAsString returns "<Mode>/<Synch|Asynch>".
// CHECKED
func getModeCombinedAsString() string {
	return OptimizeMode.String() + "/" + getSyncAsString()
}

// -------------------------
// ModeType String()
// -------------------------

// String returns the string representation of the ModeType.
// CHECKED
func (m ModeType) String() string {
	switch m {
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
		// Preserve previous behavior (unknown -> "Periodic") to avoid changing logs/tests.
		return "Periodic"
	}
}
