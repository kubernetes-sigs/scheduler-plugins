// nudge_blocked.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Only meaningful for ForEvery@PreEnqueue
// This function is needed as if we activate all blocked pods at once
// over and over again in onPlanSettled, we end up with a large waiting time in the queue.
func (pl *MyCrossNodePreemption) idleNudgeBlockedLoop(ctx context.Context) {
	if !optimizeForEvery() || !optimizeAtPreEnqueue() {
		return
	}

	base := NudgeBlockedInterval
	delay := base

	var last types.UID
	var sameCount int // consecutive activations of the same UID

	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// If caches are not warm, or a plan is active, or nothing blocked: skip and reset to base delay.
			if !pl.CachesWarm.Load() {
				klog.V(V2).InfoS("Idle nudge: caches not warmed up yet; skipping")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			if pl.IsActivePlan() {
				klog.V(V2).InfoS("Idle nudge: plan is active; skipping")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			if pl.Blocked == nil || pl.Blocked.Size() == 0 {
				klog.V(V2).InfoS("Idle nudge: no blocked pods to activate")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}

			// Wake exactly one; activateBlockedPods now returns the UIDs it *attempted* to activate.
			tried := pl.activateBlockedPods(1)

			var activated types.UID
			if len(tried) == 1 {
				activated = tried[0]
				if activated == last {
					sameCount++
				} else {
					sameCount = 0
					last = activated
				}
				klog.V(V2).InfoS("Idle nudge: activated one blocked pod",
					"uid", string(activated), "sameCount", sameCount)
			} else {
				// No activation (or ambiguous); reset backoff.
				sameCount = 0
				last = ""
				klog.V(V2).InfoS("Idle nudge: attempted activation but none selected; resetting backoff")
			}

			// Backoff: base + (sameCount * base/2), capped at 5*base (1s).
			extra := time.Duration(sameCount) * base / 2
			const maxFactor = 5
			if extra > time.Duration(maxFactor)*base {
				extra = time.Duration(maxFactor) * base
			}
			delay = base + extra

			timer.Reset(delay)
		}
	}
}
