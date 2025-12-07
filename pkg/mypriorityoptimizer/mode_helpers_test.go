// mode_helpers_test.go
package mypriorityoptimizer

import "testing"

// -----------------------------------------------------------------------------
// isPerPodMode
// -----------------------------------------------------------------------------

func TestIsPerPodMode(t *testing.T) {
	withMode(ModePerPod, StagePostFilter, true, func() {
		if !isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be true when OptimizeMode == ModePerPod")
		}
	})
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false when OptimizeMode != ModePerPod")
		}
	})
	withMode(ModeInterlude, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeInterlude")
		}
	})
	withMode(ModeManual, StagePreEnqueue, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeManual")
		}
	})
}

// -----------------------------------------------------------------------------
// isBackgroundMode
// -----------------------------------------------------------------------------

func TestIsBackgroundMode(t *testing.T) {
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if !isBackgroundMode() {
			t.Fatalf("expected isBackgroundMode() to be true for ModePeriodic")
		}
	})
	withMode(ModeInterlude, StagePreEnqueue, true, func() {
		if !isBackgroundMode() {
			t.Fatalf("expected isBackgroundMode() to be true for ModeInterlude")
		}
	})
	withMode(ModeManual, StagePreEnqueue, true, func() {
		if !isBackgroundMode() {
			t.Fatalf("expected isBackgroundMode() to be true for ModeManual")
		}
	})
	withMode(ModePerPod, StagePostFilter, true, func() {
		if isBackgroundMode() {
			t.Fatalf("expected isBackgroundMode() to be false for ModePerPod")
		}
	})
}

// -----------------------------------------------------------------------------
// isManualMode
// -----------------------------------------------------------------------------

func TestIsManualMode(t *testing.T) {
	withMode(ModeManual, StagePreEnqueue, true, func() {
		if !isManualMode() {
			t.Fatalf("expected isManualMode() to be true for ModeManual")
		}
	})
	withMode(ModePerPod, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for ModePerPod")
		}
	})
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isManualMode() {
			t.Fatalf("expected isManualMode() to be false for ModePeriodic")
		}
	})
	withMode(ModeInterlude, StagePreEnqueue, true, func() {
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
	withMode(ModePerPod, StagePostFilter, false, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false for ModePerPod regardless of OptimizeSolveSynch")
		}
	})
	// Background modes: OptimizeSolveSynch=true -> synchronous
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false when OptimizeSolveSynch=true for background non-PerPod modes")
		}
	})
	// Background modes: OptimizeSolveSynch=false -> asynchronous
	withMode(ModePeriodic, StagePreEnqueue, false, func() {
		if !isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be true when OptimizeSolveSynch=false for background non-PerPod modes")
		}
	})
}

// -----------------------------------------------------------------------------
// hookAtPreEnqueue / hookAtPostFilter
// -----------------------------------------------------------------------------

func TestHookAtStage(t *testing.T) {
	// PerPod: we force PostFilter regardless of OptimizeHookStage.
	withMode(ModePerPod, StagePreEnqueue, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false for ModePerPod")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be true for ModePerPod")
		}
	})

	// Background mode with StagePreEnqueue.
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if !hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() when OptimizeHookStage=StagePreEnqueue in background mode")
		}
		if hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() to be false when OptimizeHookStage=StagePreEnqueue in background mode")
		}
	})

	// Background mode with StagePostFilter.
	withMode(ModePeriodic, StagePostFilter, true, func() {
		if hookAtPreEnqueue() {
			t.Fatalf("expected hookAtPreEnqueue() to be false when OptimizeHookStage=StagePostFilter in background mode")
		}
		if !hookAtPostFilter() {
			t.Fatalf("expected hookAtPostFilter() when OptimizeHookStage=StagePostFilter in background mode")
		}
	})
}

// -----------------------------------------------------------------------------
// modeToString
// -----------------------------------------------------------------------------

func TestModeToString(t *testing.T) {
	withMode(ModePerPod, StagePostFilter, true, func() {
		if got := modeToString(); got != "PerPod" {
			t.Fatalf("expected modeToString()=PerPod for ModePerPod, got %q", got)
		}
	})
	withMode(ModePeriodic, StagePostFilter, true, func() {
		if got := modeToString(); got != "Periodic" {
			t.Fatalf("expected modeToString()=Periodic for ModePeriodic, got %q", got)
		}
	})
	withMode(ModeInterlude, StagePostFilter, true, func() {
		if got := modeToString(); got != "Interlude" {
			t.Fatalf("expected modeToString()=Interlude for ModeInterlude, got %q", got)
		}
	})
	withMode(ModeManual, StagePostFilter, true, func() {
		if got := modeToString(); got != "Manual" {
			t.Fatalf("expected modeToString()=Manual for ModeManual, got %q", got)
		}
	})
	// Unknown mode value -> default branch ("Periodic")
	withMode(ModeType(999), StagePostFilter, true, func() {
		if got := modeToString(); got != "Periodic" {
			t.Fatalf("expected modeToString()=Periodic for unknown mode value, got %q", got)
		}
	})
}

// -----------------------------------------------------------------------------
// stageToString
// -----------------------------------------------------------------------------

func TestStageToString(t *testing.T) {
	// PerPod: always PostFilter regardless of OptimizeHookStage.
	withMode(ModePerPod, StagePreEnqueue, true, func() {
		if got := stageToString(); got != "PostFilter" {
			t.Fatalf("expected stageToString()=PostFilter for ModePerPod, got %q", got)
		}
	})

	// Background mode with StagePreEnqueue.
	withMode(ModePeriodic, StagePreEnqueue, true, func() {
		if got := stageToString(); got != "PreEnqueue" {
			t.Fatalf("expected stageToString()=PreEnqueue for background mode with StagePreEnqueue, got %q", got)
		}
	})

	// Background mode with StagePostFilter.
	withMode(ModePeriodic, StagePostFilter, true, func() {
		if got := stageToString(); got != "PostFilter" {
			t.Fatalf("expected stageToString()=PostFilter for background mode with StagePostFilter, got %q", got)
		}
	})
}

// -----------------------------------------------------------------------------
// syncToString
// -----------------------------------------------------------------------------

func TestSyncToString(t *testing.T) {
	// PerPod is always synchronous.
	withMode(ModePerPod, StagePostFilter, false, func() {
		if got := syncToString(); got != "Synch" {
			t.Fatalf("expected syncToString()=Synch for ModePerPod, got %q", got)
		}
	})

	// Background mode, OptimizeSolveSynch=true -> synchronous.
	withMode(ModePeriodic, StagePostFilter, true, func() {
		if got := syncToString(); got != "Synch" {
			t.Fatalf("expected syncToString()=Synch when OptimizeSolveSynch=true, got %q", got)
		}
	})

	// Background mode, OptimizeSolveSynch=false -> asynchronous.
	withMode(ModePeriodic, StagePostFilter, false, func() {
		if got := syncToString(); got != "Asynch" {
			t.Fatalf("expected syncToString()=Asynch when OptimizeSolveSynch=false, got %q", got)
		}
	})
}

// -----------------------------------------------------------------------------
// combinedModeToString
// -----------------------------------------------------------------------------

func TestCombinedModeToString(t *testing.T) {
	// PerPod: always PostFilter + Synch
	withMode(ModePerPod, StagePreEnqueue, true, func() {
		got := combinedModeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected combinedModeToString for PerPod+Synch+PreEnqueue: %q", got)
		}
	})
	withMode(ModePerPod, StagePostFilter, false, func() {
		got := combinedModeToString()
		if got != "PerPod/PostFilter/Synch" {
			t.Fatalf("unexpected combinedModeToString for PerPod+Asynch+PostFilter (forced Synch): %q", got)
		}
	})

	// Periodic + Asynch + PostFilter
	withMode(ModePeriodic, StagePostFilter, false, func() {
		got := combinedModeToString()
		if got != "Periodic/PostFilter/Asynch" {
			t.Fatalf("unexpected combinedModeToString for Periodic+Asynch+PostFilter: %q", got)
		}
	})

	// Manual + Synch + PostFilter
	withMode(ModeManual, StagePostFilter, true, func() {
		got := combinedModeToString()
		if got != "Manual/PostFilter/Synch" {
			t.Fatalf("unexpected combinedModeToString for Manual+Synch+PostFilter: %q", got)
		}
	})

	// Interlude + Asynch + PreEnqueue
	withMode(ModeInterlude, StagePreEnqueue, false, func() {
		got := combinedModeToString()
		if got != "Interlude/PreEnqueue/Asynch" {
			t.Fatalf("unexpected combinedModeToString for Interlude+Asynch+PreEnqueue: %q", got)
		}
	})

	// Unknown mode value -> default branch ("Periodic")
	withMode(ModeType(999), StagePreEnqueue, true, func() {
		got := combinedModeToString()
		if got != "Periodic/PreEnqueue/Synch" {
			t.Fatalf("unexpected combinedModeToString for unknown mode value: %q", got)
		}
	})
}
