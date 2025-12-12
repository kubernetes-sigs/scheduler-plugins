// loop_periodic.go
package mypriorityoptimizer

import (
	"context"
	"time"
)

func (pl *SharedState) loopPeriodic(ctx context.Context) {
	if OptimizePeriodicInterval <= 1 {
		OptimizePeriodicInterval = 2 * time.Second
	}

	cfg := OptimizeLoopConfig{
		Label:          "PeriodicLoop",
		Interval:       OptimizePeriodicInterval,
		InterludeDelay: 0,     // no "idle window" -> behave like periodic
		CancelOnChange: false, // do NOT cancel if new pods arrive (can be made configurable later)
	}
	// delegated through hook
	optimizeBackgroundLoopFunc(pl, ctx, cfg)
}
