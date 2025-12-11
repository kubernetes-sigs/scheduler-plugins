// mode_helpers_test.go
package mypriorityoptimizer

import "testing"

// -----------------------------------------------------------------------------
// isPerPodMode
// -----------------------------------------------------------------------------

func TestIsPerPodMode(t *testing.T) {
	withMode(ModePerPod, true, func() {
		if !isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be true when OptimizeMode == ModePerPod")
		}
	})
	withMode(ModePeriodic, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false when OptimizeMode != ModePerPod")
		}
	})
	withMode(ModeInterlude, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeInterlude")
		}
	})
	withMode(ModeManual, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeManual")
		}
	})
	withMode(ModeManualBlocking, true, func() {
		if isPerPodMode() {
			t.Fatalf("expected isPerPodMode() to be false for ModeManualBlocking")
		}
	})
}

func TestIsManualBlockingMode(t *testing.T) {
	withMode(ModeManualBlocking, true, func() {
		if !isManualBlockingMode() {
			t.Fatalf("expected isManualBlockingMode() to be true for ModeManualBlocking")
		}
	})
	withMode(ModeManual, true, func() {
		if isManualBlockingMode() {
			t.Fatalf("expected isManualBlockingMode() to be false for ModeManual")
		}
	})
	withMode(ModePerPod, true, func() {
		if isManualBlockingMode() {
			t.Fatalf("expected isManualBlockingMode() to be false for ModePerPod")
		}
	})
	withMode(ModePeriodic, true, func() {
		if isManualBlockingMode() {
			t.Fatalf("expected isManualBlockingMode() to be false for ModePeriodic")
		}
	})
	withMode(ModeInterlude, true, func() {
		if isManualBlockingMode() {
			t.Fatalf("expected isManualBlockingMode() to be false for ModeInterlude")
		}
	})
}

// -----------------------------------------------------------------------------
// isAsyncSolving
// -----------------------------------------------------------------------------

func TestIsAsyncSolving(t *testing.T) {
	// PerPod is always synchronous regardless of OptimizeSolveSynch
	withMode(ModePerPod, false, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false for ModePerPod regardless of OptimizeSolveSynch")
		}
	})
	// Background modes: OptimizeSolveSynch=true -> synchronous
	withMode(ModePeriodic, true, func() {
		if isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be false when OptimizeSolveSynch=true for background non-PerPod modes")
		}
	})
	// Background modes: OptimizeSolveSynch=false -> asynchronous
	withMode(ModePeriodic, false, func() {
		if !isAsyncSolving() {
			t.Fatalf("expected isAsyncSolving() to be true when OptimizeSolveSynch=false for background non-PerPod modes")
		}
	})
}

// -----------------------------------------------------------------------------
// modeToString
// -----------------------------------------------------------------------------

func TestModeToString(t *testing.T) {
	withMode(ModePerPod, true, func() {
		if got := modeToString(); got != "PerPod" {
			t.Fatalf("expected modeToString()=PerPod for ModePerPod, got %q", got)
		}
	})
	withMode(ModePeriodic, true, func() {
		if got := modeToString(); got != "Periodic" {
			t.Fatalf("expected modeToString()=Periodic for ModePeriodic, got %q", got)
		}
	})
	withMode(ModeInterlude, true, func() {
		if got := modeToString(); got != "Interlude" {
			t.Fatalf("expected modeToString()=Interlude for ModeInterlude, got %q", got)
		}
	})
	withMode(ModeManual, true, func() {
		if got := modeToString(); got != "Manual" {
			t.Fatalf("expected modeToString()=Manual for ModeManual, got %q", got)
		}
	})
	withMode(ModeManualBlocking, true, func() {
		if got := modeToString(); got != "ManualBlocking" {
			t.Fatalf("expected modeToString()=ManualBlocking for ModeManualBlocking, got %q", got)
		}
	})
	// Unknown mode value -> default branch ("Periodic")
	withMode(ModeType(999), true, func() {
		if got := modeToString(); got != "Periodic" {
			t.Fatalf("expected modeToString()=Periodic for unknown mode value, got %q", got)
		}
	})
}

// -----------------------------------------------------------------------------
// syncToString
// -----------------------------------------------------------------------------

func TestSyncToString(t *testing.T) {
	// PerPod is always synchronous.
	withMode(ModePerPod, false, func() {
		if got := syncToString(); got != "Synch" {
			t.Fatalf("expected syncToString()=Synch for ModePerPod, got %q", got)
		}
	})

	// Background mode, OptimizeSolveSynch=true -> synchronous.
	withMode(ModePeriodic, true, func() {
		if got := syncToString(); got != "Synch" {
			t.Fatalf("expected syncToString()=Synch when OptimizeSolveSynch=true, got %q", got)
		}
	})

	// Background mode, OptimizeSolveSynch=false -> asynchronous.
	withMode(ModePeriodic, false, func() {
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
	withMode(ModePerPod, true, func() {
		got := combinedModeToString()
		if got != "PerPod/Synch" {
			t.Fatalf("unexpected combinedModeToString for PerPod+Synch+PreEnqueue: %q", got)
		}
	})
	withMode(ModePerPod, false, func() {
		got := combinedModeToString()
		if got != "PerPod/Synch" {
			t.Fatalf("unexpected combinedModeToString for PerPod+Asynch+PostFilter (forced Synch): %q", got)
		}
	})

	// Periodic + Asynch + PostFilter
	withMode(ModePeriodic, false, func() {
		got := combinedModeToString()
		if got != "Periodic/Asynch" {
			t.Fatalf("unexpected combinedModeToString for Periodic+Asynch+PostFilter: %q", got)
		}
	})

	// Manual + Synch + PostFilter
	withMode(ModeManual, true, func() {
		got := combinedModeToString()
		if got != "Manual/Synch" {
			t.Fatalf("unexpected combinedModeToString for Manual+Synch+PostFilter: %q", got)
		}
	})

	// ManaulBlocking + Synch + PreEnqueue
	withMode(ModeManualBlocking, true, func() {
		got := combinedModeToString()
		if got != "ManualBlocking/Synch" {
			t.Fatalf("unexpected combinedModeToString for ManualBlocking+Synch+PreEnqueue: %q", got)
		}
	})

	// Interlude + Asynch + PreEnqueue
	withMode(ModeInterlude, false, func() {
		got := combinedModeToString()
		if got != "Interlude/Asynch" {
			t.Fatalf("unexpected combinedModeToString for Interlude+Asynch+PreEnqueue: %q", got)
		}
	})

	// Unknown mode value -> default branch ("Periodic")
	withMode(ModeType(999), true, func() {
		got := combinedModeToString()
		if got != "Periodic/Synch" {
			t.Fatalf("unexpected combinedModeToString for unknown mode value: %q", got)
		}
	})
}
