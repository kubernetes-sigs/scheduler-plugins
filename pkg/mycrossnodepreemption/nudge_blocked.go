// nudge_blocked.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// Only meaningful for ForEvery@PreEnqueue
// This function is needed as if we activate all blocked pods at once
// over and over again in onPlanSettled, we end up with a large waiting time in the queue.
func (pl *MyCrossNodePreemption) idleNudgeBlockedLoop(ctx context.Context) {
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
			// If caches are not warm, do nothing.
			if !pl.CachesWarm.Load() {
				klog.V(V2).InfoS("Idle nudge: caches not warmed up yet; skipping")
				continue
			}
			// If a plan is executing (or not fully torn down), do nothing.
			if pl.IsActivePlan() {
				klog.V(V2).InfoS("Idle nudge: plan is active; skipping")
				continue
			}
			// Wake exactly one; function already prunes stale + sorts by priority/age.
			before := pl.Blocked.Size()
			if before == 0 {
				klog.V(V2).InfoS("Idle nudge: no blocked pods to activate")
				continue
			}
			pl.activateBlockedPods(1)
			klog.V(V2).InfoS("Idle nudge: activated one blocked pod")
		}
	}
}
