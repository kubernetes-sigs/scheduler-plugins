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
	klog.InfoS(label+": first run scheduled", "in(s)", firstDelay)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			klog.InfoS(label + ": cycle started")

			// no singlePod in periodic modes
			_, _ = pl.runFlow(context.Background(), phase, nil)

			timer.Reset(interval)
			klog.InfoS(label+": next run", "in(s)", interval)
		}
	}
}
