// mode_helpers_test.go
package mypriorityoptimizer

import "testing"

// withGlobals is a small helper to temporarily set the global optimization
// knobs and restore them after the test callback returns.
func withGlobals(mode ModeType, stage StageType, synch bool, fn func()) {
	oldMode := OptimizeMode
	oldStage := OptimizeHookStage
	oldSynch := OptimizeSolveSynch

	OptimizeMode = mode
	OptimizeHookStage = stage
	OptimizeSolveSynch = synch

	defer func() {
		OptimizeMode = oldMode
		OptimizeHookStage = oldStage
		OptimizeSolveSynch = oldSynch
	}()

	fn()
}

// -----------------------------------------------------------------------------
// optimizePerPod
// -----------------------------------------------------------------------------

func TestOptimizePerPod(t *testing.T) {
	withGlobals(ModePerPod, StagePostFilter, true, func() {
		if !optimizePerPod() {
			t.Fatalf("expected optimizePerPod() to be true when OptimizeMode == ModePerPod")
		}
	})
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if optimizePerPod() {
			t.Fatalf("expected optimizePerPod() to be false when OptimizeMode != ModePerPod")
		}
	})
}

// -----------------------------------------------------------------------------
// isGlobalMode
// -----------------------------------------------------------------------------

func TestIsGlobalMode(t *testing.T) {
	withGlobals(ModePerPod, StagePostFilter, true, func() {
		if isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be false for ModePerPod")
		}
	})
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModePeriodic")
		}
	})
	withGlobals(ModeManual, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModeManual")
		}
	})
	withGlobals(ModeInterlude, StagePreEnqueue, true, func() {
		if !isGlobalMode() {
			t.Fatalf("expected isGlobalMode() to be true for ModeInterlude")
		}
	})
}

// -----------------------------------------------------------------------------
// isManualMode
// -----------------------------------------------------------------------------

func TestIsManualMode(t *testing.T) {
	withGlobals(ModeManual, StagePreEnqueue, true, func() {
		if !isManualMode() {
			t.Fatalf("expected isManualMode() to be true for ModeManual")
		}
	})
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for non-manual modes")
		}
	})
}

// -----------------------------------------------------------------------------
// optimizeAsync
// -----------------------------------------------------------------------------

func TestOptimizeAsync(t *testing.T) {
	// PerPod is always synchronous regardless of OptimizeSolveSynch
	withGlobals(ModePerPod, StagePostFilter, false, func() {
		if optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be false for ModePerPod regardless of OptimizeSolveSynch")
		}
	})
	// Global modes: OptimizeSolveSynch=true -> synchronous
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be false when OptimizeSolveSynch=true for global non-PerPod modes")
		}
	})
	// Global modes: OptimizeSolveSynch=false -> asynchronous
	withGlobals(ModePeriodic, StagePreEnqueue, false, func() {
		if !optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be true when OptimizeSolveSynch=false for global non-PerPod modes")
		}
	})
}

// -----------------------------------------------------------------------------
// hookAtStage
// -----------------------------------------------------------------------------

func TestHookAtStage(t *testing.T) {
	// PerPod: we force PostFilter regardless of OptimizeHookStage.
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false for ModePerPod")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be true for ModePerPod")
		}
	})

	// Global mode with StagePreEnqueue.
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if !hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() when OptimizeHookStage=StagePreEnqueue in global mode")
		}
		if hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be false when OptimizeHookStage=StagePreEnqueue in global mode")
		}
	})

	// Global mode with StagePostFilter.
	withGlobals(ModePeriodic, StagePostFilter, true, func() {
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
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for PerPod+Synch+PreEnqueue: %q", got)
		}
	})
	withGlobals(ModePerPod, StagePostFilter, false, func() {
		got := modeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for PerPod+Asynch+PostFilter (forced Synch): %q", got)
		}
	})

	// Periodic + Asynch + PostFilter
	withGlobals(ModePeriodic, StagePostFilter, false, func() {
		got := modeToString()
		if got != "Periodic/PostFilter/Asynch" {
			t.Fatalf("unexpected modeToString for Periodic+Asynch+PostFilter: %q", got)
		}
	})

	// Manual + Synch + PostFilter
	withGlobals(ModeManual, StagePostFilter, true, func() {
		got := modeToString()
		if got != "Manual/PostFilter/Synch" {
			t.Fatalf("unexpected modeToString for Manual+Synch+PostFilter: %q", got)
		}
	})

	// Interlude + Asynch + PreEnqueue
	//   - modeStr: "Interlude"
	//   - stageStr: hookAtPreEnqueue()=true → "PreEnqueue"
	//   - syncStr: optimizeAsync()=true → "Asynch"
	withGlobals(ModeInterlude, StagePreEnqueue, false, func() {
		got := modeToString()
		if got != "Interlude/PreEnqueue/Asynch" {
			t.Fatalf("unexpected modeToString for Interlude+Asynch+PreEnqueue: %q", got)
		}
	})

	// Unknown mode value → default branch ("Periodic")
	//   - ModeType(999) -> "Periodic"
	//   - synch: true → optimizeAsync() = false → "Synch"
	//   - stage: PreEnqueue → "PreEnqueue"
	withGlobals(ModeType(999), StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "Periodic/PreEnqueue/Synch" {
			t.Fatalf("unexpected modeToString for unknown mode value: %q", got)
		}
	})
}
