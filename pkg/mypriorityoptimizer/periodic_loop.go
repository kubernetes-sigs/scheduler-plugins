// optimize_loop.go

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// periodicLoop runs the optimization loop at a regular interval.
// It starts with an initial delay and then runs at a fixed interval.
func (pl *SharedState) periodicLoop(ctx context.Context) {
	klog.InfoS("Loop configuration", "optimizationInterval", OptimizeInterval.String())

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
				klog.InfoS(msg(strategy, InfoCachesNotWarmedUp), "nextIn", interval)
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
func (pl *SharedState) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}
	if optimizeAllSynch() || optimizeAllAsynch() {
		go pl.periodicLoop(ctx)
	} else if optimizeEvery() && optimizeAtPreEnqueue() {
		go pl.nudgeBlockedLoop(ctx)
	}
}
