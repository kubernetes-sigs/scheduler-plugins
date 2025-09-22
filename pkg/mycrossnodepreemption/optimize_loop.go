// optimize_loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// optimizeLoop runs the optimization loop at a regular interval.
// It starts with an initial delay and then runs at a fixed interval.
func (pl *MyCrossNodePreemption) optimizeLoop(ctx context.Context) {
	strategy := strategyToString()
	firstDelay := OptimizationInitialDelay
	interval := OptimizationInterval
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	klog.InfoS(strategy+": started, first run scheduled", "in", firstDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.InfoS(strategy+": caches not warmed up yet; skipping", "nextTryIn", interval)
				continue
			}
			klog.InfoS(strategy + ": cycle started")
			// no singlePod in periodic modes
			_, _ = pl.runFlow(context.Background(), nil)
			timer.Reset(interval)
			klog.InfoS(strategy+": next run", "in", interval)
		}
	}
}

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *MyCrossNodePreemption) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}
	if optimizeBatch() || optimizeContinuous() {
		go pl.optimizeLoop(ctx)
	} else if optimizeEvery() && optimizeAtPreEnqueue() {
		go pl.nudgeBlockedLoop(ctx)
	}
}
