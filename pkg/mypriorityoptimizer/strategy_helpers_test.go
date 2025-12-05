// strategy_helpers_test.go

package mypriorityoptimizer

import "testing"

// -----------------------------------------------------------------------------
// decideStrategy
// -----------------------------------------------------------------------------

func TestDecideStrategy_ActivePlanBlocks(t *testing.T) {
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		pl := &SharedState{}
		pl.ActivePlan.Store(&ActivePlan{ID: "test"})
		if got := pl.decideStrategy(StagePreEnqueue); got != DecideBlock {
			t.Fatalf("decideStrategy with active plan (PreEnqueue) = %v, want DecideBlock", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecideBlock {
			t.Fatalf("decideStrategy with active plan (PostFilter) = %v, want DecideBlock", got)
		}
	})
}

func TestDecideStrategy_ManualPreEnqueueGates(t *testing.T) {
	// Manual + PreEnqueue: gate at PreEnqueue (DecideProcessLater), pass at PostFilter.
	withGlobals(ModeManual, StagePreEnqueue, true, func() {
		pl := &SharedState{}

		if got := pl.decideStrategy(StagePreEnqueue); got != DecideProcessLater {
			t.Fatalf("manual+PreEnqueue at PreEnqueue = %v, want DecideProcessLater", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
			t.Fatalf("manual+PreEnqueue at PostFilter = %v, want DecidePass", got)
		}
	})
}

func TestDecideStrategy_ManualPostFilterPassThrough(t *testing.T) {
	// Manual + PostFilter: pass at both stages (never auto-solve).
	withGlobals(ModeManual, StagePostFilter, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
			t.Fatalf("manual+PostFilter at PreEnqueue = %v, want DecidePass", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
			t.Fatalf("manual+PostFilter at PostFilter = %v, want DecidePass", got)
		}
	})
}

func TestDecideStrategy_GlobalModesProcessLater(t *testing.T) {
	// Periodic sync: global + stage match → DecideProcessLater
	withGlobals(ModePeriodic, StagePreEnqueue, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecideProcessLater {
			t.Fatalf("periodic+PreEnqueue at PreEnqueue = %v, want DecideProcessLater", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
			t.Fatalf("periodic+PreEnqueue at PostFilter = %v, want DecidePass", got)
		}
	})

	withGlobals(ModePeriodic, StagePostFilter, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePostFilter); got != DecideProcessLater {
			t.Fatalf("periodic+PostFilter at PostFilter = %v, want DecideProcessLater", got)
		}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
			t.Fatalf("periodic+PostFilter at PreEnqueue = %v, want DecidePass", got)
		}
	})
}

func TestDecideStrategy_AsyncGlobalOnlyAtPostFilter(t *testing.T) {
	// Model the state after parseOptimizeHookStage for async periodic:
	// OptimizeMode=Periodic, OptimizeSolveSynch=false ⇒ async, and hook = PostFilter.
	withGlobals(ModePeriodic, StagePostFilter, false, func() {
		pl := &SharedState{}

		if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
			t.Fatalf("periodic async at PreEnqueue = %v, want DecidePass", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecideProcessLater {
			t.Fatalf("periodic async at PostFilter = %v, want DecideProcessLater", got)
		}
	})
}

func TestDecideStrategy_ModePerPodProcessAtConfiguredStage(t *testing.T) {
	// PerPod + PreEnqueue: process at PreEnqueue, pass at PostFilter.
	withGlobals(ModePerPod, StagePreEnqueue, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecideProcess {
			t.Fatalf("perpod+PreEnqueue at PreEnqueue = %v, want DecideProcess", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
			t.Fatalf("perpod+PreEnqueue at PostFilter = %v, want DecidePass", got)
		}
	})

	// PerPod + PostFilter: pass at PreEnqueue, process at PostFilter.
	withGlobals(ModePerPod, StagePostFilter, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
			t.Fatalf("perpod+PostFilter at PreEnqueue = %v, want DecidePass", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecideProcess {
			t.Fatalf("perpod+PostFilter at PostFilter = %v, want DecideProcess", got)
		}
	})
}

func TestDecideStrategy_DefaultPassWhenNoMatch(t *testing.T) {
	// If OptimizeHookStage == StageNone, nothing should trigger DecideProcess/ProcessLater.
	withGlobals(ModePerPod, StageNone, true, func() {
		pl := &SharedState{}
		if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
			t.Fatalf("perpod+StageNone at PreEnqueue = %v, want DecidePass", got)
		}
		if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
			t.Fatalf("perpod+StageNone at PostFilter = %v, want DecidePass", got)
		}
	})
}
