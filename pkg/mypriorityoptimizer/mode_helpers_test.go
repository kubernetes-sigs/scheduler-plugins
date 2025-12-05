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
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
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
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
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
	withGlobals(ModePerPod, StagePreEnqueue, false, func() {
		if optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be false for ModePerPod regardless of OptimizeSolveSynch")
		}
	})
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		if optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be false when OptimizeSolveSynch=true for global non-PerPod modes")
		}
	})
	withGlobals(ModePeriodic, StagePreEnqueue, false, func() {
		if !optimizeAsync() {
			t.Fatalf("expected optimizeAsync() to be true when OptimizeSolveSynch=false for global non-PerPod modes")
		}
	})
}

// -----------------------------------------------------------------------------
// stage helpers
// -----------------------------------------------------------------------------

func TestStageHelpers(t *testing.T) {
	if !StagePreEnqueue.atPreEnqueue() {
		t.Fatalf("StagePreEnqueue.atPreEnqueue() expected true")
	}
	if StagePreEnqueue.atPostFilter() {
		t.Fatalf("StagePreEnqueue.atPostFilter() expected false")
	}
	if !StagePostFilter.atPostFilter() {
		t.Fatalf("StagePostFilter.atPostFilter() expected true")
	}
	if StagePostFilter.atPreEnqueue() {
		t.Fatalf("StagePostFilter.atPreEnqueue() expected false")
	}
}

// -----------------------------------------------------------------------------
// hookAtStage
// -----------------------------------------------------------------------------

func TestHookAtStage(t *testing.T) {
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		if !hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() when OptimizeHookStage=StagePreEnqueue")
		}
		if hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be false when OptimizeHookStage=StagePreEnqueue")
		}
	})
	withGlobals(ModePerPod, StagePostFilter, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false when OptimizeHookStage=StagePostFilter")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected optimizeAtPostFilter() when OptimizeHookStage=StagePostFilter")
		}
	})
}

// -----------------------------------------------------------------------------
// modeToString
// -----------------------------------------------------------------------------

func TestModeToString(t *testing.T) {
	// PerPod + Synch + PreEnqueue
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "PerPodSynch/PreEnqueue" {
			t.Fatalf("unexpected modeToString for PerPod+Synch+PreEnqueue: %q", got)
		}
	})

	// Periodic + Asynch + PostFilter
	withGlobals(ModePeriodic, StagePostFilter, false, func() {
		got := modeToString()
		if got != "PeriodicAsynch/PostFilter" {
			t.Fatalf("unexpected modeToString for Periodic+Asynch+PostFilter: %q", got)
		}
	})

	// Manual + Synch + PostFilter
	withGlobals(ModeManual, StagePostFilter, true, func() {
		got := modeToString()
		if got != "ManualSynch/PostFilter" {
			t.Fatalf("unexpected modeToString for Manual+Synch+PostFilter: %q", got)
		}
	})

	// Interlude + Asynch + PreEnqueue
	//   - modeStr branch: ModeInterlude -> "Interlude"
	//   - syncStr: OptimizeSolveSynch=false → optimizeAsync()=true → "Asynch"
	//   - stageStr: StagePreEnqueue → "PreEnqueue"
	withGlobals(ModeInterlude, StagePreEnqueue, false, func() {
		got := modeToString()
		if got != "InterludeAsynch/PreEnqueue" {
			t.Fatalf("unexpected modeToString for Interlude+Asynch+PreEnqueue: %q", got)
		}
	})

	// Unknown mode value → default branch ("Periodic")
	//   - ModeType(999) hits the default: modeStr = "Periodic"
	//   - synch: true → optimizeAsync() = false → "Synch"
	//   - stage: PreEnqueue → "PreEnqueue"
	withGlobals(ModeType(999), StagePreEnqueue, true, func() {
		got := modeToString()
		if got != "PeriodicSynch/PreEnqueue" {
			t.Fatalf("unexpected modeToString for unknown mode value: %q", got)
		}
	})
}
