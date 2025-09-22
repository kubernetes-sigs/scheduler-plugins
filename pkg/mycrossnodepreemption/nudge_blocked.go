// nudge_blocked.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Only meaningful for Every@PreEnqueue
// This function is needed as if we activate all blocked pods at once
// over and over again in onPlanSettled, we end up with a large waiting time in the queue.
func (pl *MyCrossNodePreemption) nudgeBlockedLoop(ctx context.Context) {
	phaseLabel := "NudgeBlockedLoop"

	if !optimizeEvery() || !optimizeAtPreEnqueue() {
		return
	}
	strategy := strategyToString()
	klog.InfoS(phaseLabel + ": started for " + strategy)

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
				klog.V(MyV).InfoS(phaseLabel + ": caches not warmed up yet; skipping")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			ap := pl.getActivePlan()
			if ap != nil {
				klog.V(MyV).InfoS(phaseLabel + ": " + InfoActivePlanInProgress + "; skipping")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			if pl.BlockedWhileActive == nil || pl.BlockedWhileActive.Size() == 0 {
				klog.V(MyV).InfoS(phaseLabel + ": " + InfoNoBlockedPods + "; skipping")
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}

			// Wake exactly one
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
				klog.V(MyV).InfoS(phaseLabel+": activated one blocked pod",
					"uid", string(activated), "sameCount", sameCount)
			} else {
				// No activation; reset backoff
				sameCount = 0
				last = ""
				klog.V(MyV).InfoS(phaseLabel + ": attempted activation but none selected; resetting backoff")
			}

			// Backoff: base + (sameCount * base/2), capped at 5*base (1s)
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
