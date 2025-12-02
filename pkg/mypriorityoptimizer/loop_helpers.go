package mypriorityoptimizer

import (
	"context"
)

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *SharedState) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}

	switch OptimizeMode {
	case ModeAllSynch, ModeAllAsynch:
		// Fixed-interval global optimization.
		go pl.periodicLoop(ctx)

	case ModeFreeTimeSynch, ModeFreeTimeAsynch:
		// Free-time global optimization (runs only when the queue is quiescent).
		go pl.freetimeLoop(ctx)

	default:
		// Every@PreEnqueue uses the nudgeBlockedLoop helper.
		if optimizeEvery() && optimizeAtPreEnqueue() {
			go pl.nudgeBlockedLoop(ctx)
		}
	}
}
