// mode_helpers_test.go
package mypriorityoptimizer

import "testing"

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// withChosenMode is a small helper to temporarily set the mode during a test.
func withChosenMode(mode ModeType, stage StageType, synch bool, fn func()) {
	OptimizeMode = mode
	OptimizeHookStage = stage
	OptimizeSolveSynch = synch
	fn()
}

// -----------------------------------------------------------------------------
// isPerPodMode
// -----------------------------------------------------------------------------

func TestIsPerPodMode(t *testing.T) {
	withChosenMode(ModePerPod, StagePostFilter, true, func() {
		if !isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be true when OptimizeMode == ModePerPod")
		}
	})
	withChosenMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false when OptimizeMode != ModePerPod")
		}
	})
	withChosenMode(ModeInterlude, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeInterlude")
		}
	})
	withChosenMode(ModeManual, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeManual")
		}
	})
}

// -----------------------------------------------------------------------------
// isGlobalMode
// -----------------------------------------------------------------------------

func TestIsGlobalMode(t *testing.T) {
	withChosenMode(ModePeriodic, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModePeriodic")
		}
	})
	withChosenMode(ModeInterlude, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModeInterlude")
		}
	})
	withChosenMode(ModeManual, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModeManual")
		}
	})
	withChosenMode(ModePerPod, StagePostFilter, true, func() {
		if isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be false for ModePerPod")
		}
	})
}

// -----------------------------------------------------------------------------
// isManualMode
// -----------------------------------------------------------------------------

func TestIsManualMode(t *testing.T) {
	withChosenMode(ModeManual, StagePreEnqueue, true, func() {
		if !isManualMode() {
			t.Fatalf("expected isManualMode() to be true for ModeManual")
		}
	})
	withChosenMode(ModePerPod, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for ModePerPod")
		}
	})
	withChosenMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for ModePeriodic")
		}
	})
	withChosenMode(ModeInterlude, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for ModeInterlude")
		}
	})
}

// -----------------------------------------------------------------------------
// isAsyncSolving
// -----------------------------------------------------------------------------

func TestIsAsyncSolving(t *testing.T) {
	// PerPod is always synchronous regardless of OptimizeSolveSynch
	withChosenMode(ModePerPod, StagePostFilter, false, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false for ModePerPod regardless of OptimizeSolveSynch")
		}
	})
	// Global modes: OptimizeSolveSynch=true -> synchronous
	withChosenMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false when OptimizeSolveSynch=true for global non-PerPod modes")
		}
	})
	// Global modes: OptimizeSolveSynch=false -> asynchronous
	withChosenMode(ModePeriodic, StagePreEnqueue, false, func() {
		if !isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be true when OptimizeSolveSynch=false for global non-PerPod modes")
		}
	})
}

// -----------------------------------------------------------------------------
// hookAtStage
// -----------------------------------------------------------------------------

func TestHookAtStage(t *testing.T) {
	// PerPod: we force PostFilter regardless of OptimizeHookStage.
	withChosenMode(ModePerPod, StagePreEnqueue, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false for ModePerPod")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be true for ModePerPod")
		}
	})

	// Global mode with StagePreEnqueue.
	withChosenMode(ModePeriodic, StagePreEnqueue, true, func() {
		if !hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() when OptimizeHookStage=StagePreEnqueue in global mode")
		}
		if hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be false when OptimizeHookStage=StagePreEnqueue in global mode")
		}
	})

	// Global mode with StagePostFilter.
	withChosenMode(ModePeriodic, StagePostFilter, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false when OptimizeHookStage=StagePostFilter in global mode")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() when OptimizeHookStage=StagePostFilter in global mode")
		}
	})
}

// -----------------------------------------------------------------------------
// modeToString
// -----------------------------------------------------------------------------

func TestModeToString(t *testing.T) {
	// PerPod: always PostFilter + Synch, regardless of stage & OptimizeSolveSynch.
	withChosenMode(ModePerPod, StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for PerPod+Synch+PreEnqueue: %q", got)
		}
	})
	withChosenMode(ModePerPod, StagePostFilter, false, func() {
		got := modeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for PerPod+Asynch+PostFilter (forced Synch): %q", got)
		}
	})

	// Periodic + Asynch + PostFilter
	withChosenMode(ModePeriodic, StagePostFilter, false, func() {
		got := modeToString()
		if got != "Periodic/PostFilter/Asynch" {
			t.Fatalf("unexpected modeToString for Periodic+Asynch+PostFilter: %q", got)
		}
	})

	// Manual + Synch + PostFilter
	withChosenMode(ModeManual, StagePostFilter, true, func() {
		got := modeToString()
		if got != "Manual/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for Manual+Synch+PostFilter: %q", got)
		}
	})

	// Interlude + Asynch + PreEnqueue
	withChosenMode(ModeInterlude, StagePreEnqueue, false, func() {
		got := modeToString()
		if got != "Interlude/PreEnqueue/Asynch" {
			t.Fatalf("unexpected modeToString for Interlude+Asynch+PreEnqueue: %q", got)
		}
	})

	// Unknown mode value -> default branch ("Periodic")
	//   - ModeType(999) -> "Periodic"
	//   - synch: true -> optimizeAsync() = false -> "Synch"
	//   - stage: PreEnqueue -> "PreEnqueue"
	withChosenMode(ModeType(999), StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "Periodic/PreEnqueue/Synch" {
			t.Fatalf("unexpected modeToString for unknown mode value: %q", got)
		}
	})
}
