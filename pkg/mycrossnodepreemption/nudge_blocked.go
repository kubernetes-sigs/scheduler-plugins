// nudge_blocked.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

const (
	// How often to try waking one blocked pod when idle in ForEvery@PreEnqueue.
	NudgeBlockedInterval = 200 * time.Millisecond
)

func (pl *MyCrossNodePreemption) idleNudgeBlockedLoop(ctx context.Context) {
	// Only meaningful for ForEvery@PreEnqueue
	// This function is needed as if we activate all blocked pods at once
	// over and over again in onPlanSettled, we end up with a large waiting time.
	if !optimizeForEvery() || !optimizeAtPreEnqueue() {
		return
	}
	t := time.NewTicker(NudgeBlockedInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// If a plan is executing (or not fully torn down), do nothing.
			if pl.Active.Load() {
				continue
			}
			// Wake exactly one; function already prunes stale + sorts by priority/age.
			before := pl.Blocked.Size()
			if before == 0 {
				continue
			}
			pl.activateBlockedPods(1)
			klog.V(V2).InfoS("Idle nudge: activated one blocked pod")
		}
	}
}
