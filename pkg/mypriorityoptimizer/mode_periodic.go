// mode_periodic.go

package mypriorityoptimizer

import "context"

func (pl *SharedState) modePeriodic(ctx context.Context) {
	cfg := optimizeLoopConfig{
		Label:          "PeriodicLoop",
		Interval:       OptimizeInterval,
		InterludeDelay: 0,     // no "idle window" → behave like periodic
		CancelOnChange: false, // do NOT cancel if new pods arrive (can be made configurable later)
	}
	pl.optimizeBackgroundLoop(ctx, cfg)
}
