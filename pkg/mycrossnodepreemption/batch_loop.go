// batch_loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	firstDelay := OptimizationInitialDelay
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	klog.InfoS("Batch loop: first run scheduled", "in(s)", firstDelay)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			klog.InfoS("Batch loop: cycle")
			_, _ = pl.runFlow(context.Background(), PhaseBatch, nil)
			timer.Reset(OptimizationInterval)
			klog.InfoS("Batch loop: next run", "in(s)", OptimizationInterval)
		}
	}
}
