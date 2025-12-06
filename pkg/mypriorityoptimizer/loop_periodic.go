// loop_periodic.go
package mypriorityoptimizer

import (
	"context"
	"time"
)

func (pl *SharedState) loopPeriodic(ctx context.Context) {
	if OptimizeInterval <= 1 {
		OptimizeInterval = 2 * time.Second
	}

	cfg := optimizeLoopConfig{
		Label:          "PeriodicLoop",
		Interval:       OptimizeInterval,
		InterludeDelay: 0,     // no "idle window" → behave like periodic
		CancelOnChange: false, // do NOT cancel if new pods arrive (can be made configurable later)
	}
	// delegated through hook
	optimizeGlobalBackgroundLoopFunc(pl, ctx, cfg)
}
