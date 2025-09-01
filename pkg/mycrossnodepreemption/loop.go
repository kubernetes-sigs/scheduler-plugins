// loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

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
			klog.InfoS(label + ": cycle started")

			if !pl.CachesWarm.Load() {
				klog.InfoS(label+": caches not warmed up yet; skipping", "nextTryIn", interval)
				continue
			}

			// no singlePod in periodic modes
			_, _ = pl.runFlow(context.Background(), phase, nil)

			timer.Reset(interval)
			klog.InfoS(label+": next run", "in", interval)
		}
	}
}
