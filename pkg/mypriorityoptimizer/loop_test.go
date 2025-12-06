// loop_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"
	"time"
)

// helper to temporarily override optimizeBackgroundLoopFunc
func withOptimizeLoopFunc(t *testing.T,
	fn func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig),
	body func(),
) {
	t.Helper()
	orig := optimizeBackgroundLoopFunc
	optimizeBackgroundLoopFunc = fn
	defer func() { optimizeBackgroundLoopFunc = orig }()
	body()
}

func TestLoopInterlude_UsesDefaultsWhenNonPositive(t *testing.T) {
	origDelay := OptimizeInterludeDelay
	origCheck := OptimizeInterludeCheckInterval
	defer func() {
		OptimizeInterludeDelay = origDelay
		OptimizeInterludeCheckInterval = origCheck
	}()

	OptimizeInterludeDelay = 0
	OptimizeInterludeCheckInterval = 0

	var gotCfg OptimizeLoopConfig
	var called bool

	withOptimizeLoopFunc(t,
		func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
			called = true
			gotCfg = cfg
		},
		func() {
			pl := &SharedState{}
			pl.loopInterlude(context.Background())
		},
	)

	if !called {
		t.Fatalf("loopInterlude() did not call optimizeBackgroundLoopFunc")
	}
	if gotCfg.Label != "InterludeLoop" {
		t.Fatalf("cfg.Label = %q, want %q", gotCfg.Label, "InterludeLoop")
	}
	if gotCfg.Interval != 250*time.Millisecond {
		t.Fatalf("cfg.Interval = %v, want %v", gotCfg.Interval, 250*time.Millisecond)
	}
	if gotCfg.InterludeDelay != 2*time.Second {
		t.Fatalf("cfg.InterludeDelay = %v, want %v", gotCfg.InterludeDelay, 2*time.Second)
	}
	if !gotCfg.CancelOnChange {
		t.Fatalf("cfg.CancelOnChange = false, want true")
	}
}

func TestLoopInterlude_UsesConfiguredValues(t *testing.T) {
	origDelay := OptimizeInterludeDelay
	origCheck := OptimizeInterludeCheckInterval
	defer func() {
		OptimizeInterludeDelay = origDelay
		OptimizeInterludeCheckInterval = origCheck
	}()

	OptimizeInterludeDelay = 5 * time.Second
	OptimizeInterludeCheckInterval = 123 * time.Millisecond

	var gotCfg OptimizeLoopConfig
	var called bool

	withOptimizeLoopFunc(t,
		func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
			called = true
			gotCfg = cfg
		},
		func() {
			pl := &SharedState{}
			pl.loopInterlude(context.Background())
		},
	)

	if !called {
		t.Fatalf("loopInterlude() did not call optimizeBackgroundLoopFunc")
	}
	if gotCfg.Label != "InterludeLoop" {
		t.Fatalf("cfg.Label = %q, want %q", gotCfg.Label, "InterludeLoop")
	}
	if gotCfg.Interval != 123*time.Millisecond {
		t.Fatalf("cfg.Interval = %v, want %v", gotCfg.Interval, 123*time.Millisecond)
	}
	if gotCfg.InterludeDelay != 5*time.Second {
		t.Fatalf("cfg.InterludeDelay = %v, want %v", gotCfg.InterludeDelay, 5*time.Second)
	}
	if !gotCfg.CancelOnChange {
		t.Fatalf("cfg.CancelOnChange = false, want true")
	}
}

func TestLoopPeriodic_DefaultIntervalWhenTooSmall(t *testing.T) {
	origInterval := OptimizeInterval
	defer func() { OptimizeInterval = origInterval }()

	// Trigger default path
	OptimizeInterval = 0

	var gotCfg OptimizeLoopConfig
	var called bool

	withOptimizeLoopFunc(t,
		func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
			called = true
			gotCfg = cfg
		},
		func() {
			pl := &SharedState{}
			pl.loopPeriodic(context.Background())
		},
	)

	if !called {
		t.Fatalf("loopPeriodic() did not call optimizeBackgroundLoopFunc")
	}

	// loopPeriodic mutates OptimizeInterval when it is too small
	if OptimizeInterval != 2*time.Second {
		t.Fatalf("OptimizeInterval after loopPeriodic = %v, want %v", OptimizeInterval, 2*time.Second)
	}

	if gotCfg.Label != "PeriodicLoop" {
		t.Fatalf("cfg.Label = %q, want %q", gotCfg.Label, "PeriodicLoop")
	}
	if gotCfg.Interval != 2*time.Second {
		t.Fatalf("cfg.Interval = %v, want %v", gotCfg.Interval, 2*time.Second)
	}
	if gotCfg.InterludeDelay != 0 {
		t.Fatalf("cfg.InterludeDelay = %v, want 0", gotCfg.InterludeDelay)
	}
	if gotCfg.CancelOnChange {
		t.Fatalf("cfg.CancelOnChange = true, want false")
	}
}

func TestLoopPeriodic_UsesConfiguredInterval(t *testing.T) {
	origInterval := OptimizeInterval
	defer func() { OptimizeInterval = origInterval }()

	OptimizeInterval = 5 * time.Second

	var gotCfg OptimizeLoopConfig
	var called bool

	withOptimizeLoopFunc(t,
		func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
			called = true
			gotCfg = cfg
		},
		func() {
			pl := &SharedState{}
			pl.loopPeriodic(context.Background())
		},
	)

	if !called {
		t.Fatalf("loopPeriodic() did not call optimizeBackgroundLoopFunc")
	}

	// Should not have been overwritten
	if OptimizeInterval != 5*time.Second {
		t.Fatalf("OptimizeInterval after loopPeriodic = %v, want %v", OptimizeInterval, 5*time.Second)
	}

	if gotCfg.Label != "PeriodicLoop" {
		t.Fatalf("cfg.Label = %q, want %q", gotCfg.Label, "PeriodicLoop")
	}
	if gotCfg.Interval != 5*time.Second {
		t.Fatalf("cfg.Interval = %v, want %v", gotCfg.Interval, 5*time.Second)
	}
	if gotCfg.InterludeDelay != 0 {
		t.Fatalf("cfg.InterludeDelay = %v, want 0", gotCfg.InterludeDelay)
	}
	if gotCfg.CancelOnChange {
		t.Fatalf("cfg.CancelOnChange = true, want false")
	}
}
