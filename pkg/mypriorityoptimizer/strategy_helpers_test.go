package mypriorityoptimizer

import "testing"

func TestOptimizeModeHelpers(t *testing.T) {
	origMode := OptimizeMode
	defer func() { OptimizeMode = origMode }()

	OptimizeMode = ModeEvery
	if !optimizeEvery() || optimizeAllSynch() || optimizeAllAsynch() || optimizeManualAllSynch() {
		t.Fatalf("ModeEvery: expected optimizeEvery=true and others=false")
	}

	OptimizeMode = ModeAllSynch
	if optimizeEvery() || !optimizeAllSynch() || optimizeAllAsynch() || optimizeManualAllSynch() {
		t.Fatalf("ModeAllSynch: expected optimizeAllSynch=true and others=false")
	}

	OptimizeMode = ModeAllAsynch
	if optimizeEvery() || optimizeAllSynch() || !optimizeAllAsynch() || optimizeManualAllSynch() {
		t.Fatalf("ModeAllAsynch: expected optimizeAllAsynch=true and others=false")
	}

	OptimizeMode = ModeManualAllSynch
	if optimizeEvery() || optimizeAllSynch() || optimizeAllAsynch() || !optimizeManualAllSynch() {
		t.Fatalf("ModeManualAllSynch: expected optimizeManualAllSynch=true and others=false")
	}
}

func TestOptimizeHookStageHelpers(t *testing.T) {
	origStage := OptimizeHookStage
	defer func() { OptimizeHookStage = origStage }()

	OptimizeHookStage = StagePreEnqueue
	if !optimizeAtPreEnqueue() || optimizeAtPostFilter() {
		t.Fatalf("StagePreEnqueue: expected optimizeAtPreEnqueue=true, optimizeAtPostFilter=false")
	}

	OptimizeHookStage = StagePostFilter
	if optimizeAtPreEnqueue() || !optimizeAtPostFilter() {
		t.Fatalf("StagePostFilter: expected optimizeAtPreEnqueue=false, optimizeAtPostFilter=true")
	}
}

func TestStageTypeMethods(t *testing.T) {
	if !StagePreEnqueue.atPreEnqueue() || StagePreEnqueue.atPostFilter() {
		t.Fatalf("StagePreEnqueue.atPreEnqueue/postFilter mismatch")
	}
	if StagePostFilter.atPreEnqueue() || !StagePostFilter.atPostFilter() {
		t.Fatalf("StagePostFilter.atPreEnqueue/postFilter mismatch")
	}
}

func TestStrategyToString_NonAsynchModes(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	// Every@PreEnqueue
	OptimizeMode = ModeEvery
	OptimizeHookStage = StagePreEnqueue
	if got := strategyToString(); got != "Every/PreEnqueue" {
		t.Fatalf("strategyToString() = %q, want %q", got, "Every/PreEnqueue")
	}

	// Every@PostFilter
	OptimizeMode = ModeEvery
	OptimizeHookStage = StagePostFilter
	if got := strategyToString(); got != "Every/PostFilter" {
		t.Fatalf("strategyToString() = %q, want %q", got, "Every/PostFilter")
	}

	// ManualAllSynch@PostFilter
	OptimizeMode = ModeManualAllSynch
	OptimizeHookStage = StagePostFilter
	if got := strategyToString(); got != "ManualAllSynch/PostFilter" {
		t.Fatalf("strategyToString() = %q, want %q", got, "ManualAllSynch/PostFilter")
	}

	// Default branch (unknown mode falls back to AllSynch)
	OptimizeMode = OptimizeModeType(9999) // invalid value to hit default
	OptimizeHookStage = StageNone
	if got := strategyToString(); got != "AllSynch/PreEnqueue" {
		t.Fatalf("strategyToString() default = %q, want %q", got, "AllSynch/PreEnqueue")
	}
}

func TestStrategyToString_AllAsynchIgnoresStage(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeAllAsynch

	OptimizeHookStage = StagePreEnqueue
	if got := strategyToString(); got != "AllAsynch" {
		t.Fatalf("strategyToString() AllAsynch/PreEnqueue = %q, want %q", got, "AllAsynch")
	}

	OptimizeHookStage = StagePostFilter
	if got := strategyToString(); got != "AllAsynch" {
		t.Fatalf("strategyToString() AllAsynch/PostFilter = %q, want %q", got, "AllAsynch")
	}
}

func TestParseOptimizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want OptimizeModeType
	}{
		{"every", ModeEvery},
		{"EVERY", ModeEvery},
		{" all_synch ", ModeAllSynch},
		{"all_asynch", ModeAllAsynch},
		{"manual_all_synch", ModeManualAllSynch},
		{"unknown", ModeAllSynch}, // default
	}

	for _, tt := range cases {
		if got := parseOptimizeMode(tt.in); got != tt.want {
			t.Fatalf("parseOptimizeMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseOptimizeHookStage_RespectsMode(t *testing.T) {
	origMode := OptimizeMode
	defer func() { OptimizeMode = origMode }()

	// Non-AllAsynch: respect input string
	OptimizeMode = ModeEvery

	if got := parseOptimizeHookStage("preenqueue"); got != StagePreEnqueue {
		t.Fatalf("parseOptimizeHookStage('preenqueue') = %v, want %v", got, StagePreEnqueue)
	}
	if got := parseOptimizeHookStage(" PostFilter "); got != StagePostFilter {
		t.Fatalf("parseOptimizeHookStage('PostFilter') = %v, want %v", got, StagePostFilter)
	}
	if got := parseOptimizeHookStage("other"); got != StageNone {
		t.Fatalf("parseOptimizeHookStage('other') = %v, want %v", got, StageNone)
	}

	// AllAsynch: always PostFilter regardless of input
	OptimizeMode = ModeAllAsynch
	if got := parseOptimizeHookStage("preenqueue"); got != StagePostFilter {
		t.Fatalf("AllAsynch: parseOptimizeHookStage('preenqueue') = %v, want %v", got, StagePostFilter)
	}
	if got := parseOptimizeHookStage("postfilter"); got != StagePostFilter {
		t.Fatalf("AllAsynch: parseOptimizeHookStage('postfilter') = %v, want %v", got, StagePostFilter)
	}
}

func TestDecideStrategy_ActivePlanBlocks(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeEvery
	OptimizeHookStage = StagePreEnqueue

	pl := &SharedState{}
	pl.ActivePlan.Store(&ActivePlan{ID: "plan-1"})

	if got := pl.decideStrategy(StagePreEnqueue); got != DecideBlock {
		t.Fatalf("decideStrategy() with active plan = %v, want DecideBlock", got)
	}
	if got := pl.decideStrategy(StagePostFilter); got != DecideBlock {
		t.Fatalf("decideStrategy() with active plan (postfilter) = %v, want DecideBlock", got)
	}
}

func TestDecideStrategy_AllSynchPreEnqueue(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeAllSynch
	OptimizeHookStage = StagePreEnqueue
	pl := &SharedState{}

	if got := pl.decideStrategy(StagePreEnqueue); got != DecideProcessLater {
		t.Fatalf("AllSynch@PreEnqueue / PreEnqueue = %v, want DecideProcessLater", got)
	}
	if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
		t.Fatalf("AllSynch@PreEnqueue / PostFilter = %v, want DecidePass", got)
	}
}

func TestDecideStrategy_AllSynchPostFilter(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeAllSynch
	OptimizeHookStage = StagePostFilter
	pl := &SharedState{}

	if got := pl.decideStrategy(StagePostFilter); got != DecideProcessLater {
		t.Fatalf("AllSynch@PostFilter / PostFilter = %v, want DecideProcessLater", got)
	}
	if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
		t.Fatalf("AllSynch@PostFilter / PreEnqueue = %v, want DecidePass", got)
	}
}

func TestDecideStrategy_AllAsynch(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeAllAsynch
	OptimizeHookStage = StagePreEnqueue // ignored by logic
	pl := &SharedState{}

	if got := pl.decideStrategy(StagePostFilter); got != DecideProcessLater {
		t.Fatalf("AllAsynch / PostFilter = %v, want DecideProcessLater", got)
	}
	if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
		t.Fatalf("AllAsynch / PreEnqueue = %v, want DecidePass", got)
	}
}

func TestDecideStrategy_EveryPreEnqueue(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeEvery
	OptimizeHookStage = StagePreEnqueue
	pl := &SharedState{}

	if got := pl.decideStrategy(StagePreEnqueue); got != DecideProcess {
		t.Fatalf("Every@PreEnqueue / PreEnqueue = %v, want DecideProcess", got)
	}
	if got := pl.decideStrategy(StagePostFilter); got != DecidePass {
		t.Fatalf("Every@PreEnqueue / PostFilter = %v, want DecidePass", got)
	}
}

func TestDecideStrategy_EveryPostFilter(t *testing.T) {
	origMode := OptimizeMode
	origStage := OptimizeHookStage
	defer func() {
		OptimizeMode = origMode
		OptimizeHookStage = origStage
	}()

	OptimizeMode = ModeEvery
	OptimizeHookStage = StagePostFilter
	pl := &SharedState{}

	if got := pl.decideStrategy(StagePostFilter); got != DecideProcess {
		t.Fatalf("Every@PostFilter / PostFilter = %v, want DecideProcess", got)
	}
	if got := pl.decideStrategy(StagePreEnqueue); got != DecidePass {
		t.Fatalf("Every@PostFilter / PreEnqueue = %v, want DecidePass", got)
	}
}
