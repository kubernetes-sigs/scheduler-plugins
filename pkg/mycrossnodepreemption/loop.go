// loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// periodicOptimizeLoop runs the optimization loop at a regular interval.
// It starts with an initial delay and then runs at a fixed interval.
func (pl *MyCrossNodePreemption) periodicOptimizeLoop(ctx context.Context) {
	label := strategyToString()
	firstDelay := OptimizationInitialDelay
	interval := OptimizationInterval
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	klog.InfoS(label+": first run scheduled", "in", firstDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.InfoS(label+": caches not warmed up yet; skipping", "nextTryIn", interval)
				continue
			}
			klog.InfoS(label + ": cycle started")
			// no singlePod in periodic modes
			_, _ = pl.runFlow(context.Background(), nil)
			timer.Reset(interval)
			klog.InfoS(label+": next run", "in", interval)
		}
	}
}

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *MyCrossNodePreemption) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}
	if !pl.LoopsStarted.CompareAndSwap(false, true) {
		return // already started
	}
	if optimizeBatch() {
		klog.InfoS("Loop: periodicOptimizeLoop started for Batch")
		go pl.periodicOptimizeLoop(ctx)
	} else if optimizeContinuous() {
		klog.InfoS("Loop: periodicOptimizeLoop started for Continuous")
		go pl.periodicOptimizeLoop(ctx)
	} else if optimizeEvery() && optimizeAtPreEnqueue() {
		klog.InfoS("Loop: nudgeBlockedLoop started for Every@PreEnqueue")
		go pl.nudgeBlockedLoop(ctx)
	}
}
