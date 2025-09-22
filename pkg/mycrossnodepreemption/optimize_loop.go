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
	firstDelay := OptimizeInitialDelay
	interval := OptimizeInterval
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	klog.InfoS(msg(strategy, InfoCycleStartedFirstRun), "in", firstDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.InfoS(msg(strategy, InfoCachesNotWarmedUp), "nextTryIn", interval)
				continue
			}
			pods, _ := pl.getPods()
			pendingCount := countPendingPods(pods)
			klog.InfoS(msg(strategy, InfoCycleStarted), "pendingPods", pendingCount)
			// no singlePod in periodic modes
			pl.runFlow(context.Background(), nil)
			timer.Reset(interval)
			klog.InfoS(msg(strategy, InfoCycleNextRun), "in", interval)
		}
	}
}

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *MyCrossNodePreemption) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}
	if optimizeAllSynch() || optimizeAllAsynch() {
		go pl.optimizeLoop(ctx)
	} else if optimizeEvery() && optimizeAtPreEnqueue() {
		go pl.nudgeBlockedLoop(ctx)
	}
}
