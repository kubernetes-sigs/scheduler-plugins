// loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// periodicOptimizeLoop runs the optimization loop at a regular interval.
// It starts with an initial delay and then runs at a fixed interval.
func (pl *MyCrossNodePreemption) periodicOptimizeLoop(ctx context.Context, phase Phase) {
	firstDelay := OptimizationInitialDelay
	interval := OptimizationInterval
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	label := string(phase)
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
			_, _ = pl.runFlow(context.Background(), phase, nil)
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
	if optimizeInBatches() {
		klog.InfoS("Loop: periodicOptimizeLoop started for InBatches")
		go pl.periodicOptimizeLoop(ctx, PhaseBatch)
	} else if optimizeContinuously() {
		klog.InfoS("Loop: periodicOptimizeLoop started for Continuously")
		go pl.periodicOptimizeLoop(ctx, PhaseContinuous)
	} else if optimizeForEvery() && optimizeAtPreEnqueue() {
		klog.InfoS("Loop: nudgeBlockedLoop started for ForEvery@PreEnqueue")
		go pl.nudgeBlockedLoop(ctx)
	}
}
