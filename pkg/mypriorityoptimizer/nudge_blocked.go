// nudge_blocked.go

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Only meaningful for Every@PreEnqueue
// This function is needed as if we activate all blocked pods at once
// over and over again in onPlanSettled, we end up with a large waiting time in the queue.
func (pl *SharedState) nudgeBlockedLoop(ctx context.Context) {
	label := "NudgeBlockedLoop"

	if !optimizeEvery() || !optimizeAtPreEnqueue() {
		return
	}
	strategy := strategyToString()
	klog.InfoS(msg(label, "started for "+strategy))

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
				klog.V(MyV).InfoS(msg(label, InfoCachesNotWarmedUp))
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			ap := pl.getActivePlan()
			if ap != nil {
				klog.V(MyV).InfoS(msg(label, InfoActivePlanInProgress))
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}
			pl.pruneSet(pl.BlockedWhileActive)
			if pl.BlockedWhileActive == nil || pl.BlockedWhileActive.Size() == 0 {
				klog.V(MyV).InfoS(msg(label, InfoNoBlockedPods))
				sameCount = 0
				last = ""
				delay = base
				timer.Reset(delay)
				continue
			}

			// Wake exactly one
			tried := pl.activatePods(pl.BlockedWhileActive, true, 1)

			var activated types.UID
			if len(tried) == 1 {
				activated = tried[0]
				if activated == last {
					sameCount++
				} else {
					sameCount = 0
					last = activated
				}
				klog.V(MyV).InfoS(msg(label, "activated one blocked pod"),
					"uid", string(activated), "sameCount", sameCount)
			} else {
				// No activation; reset backoff
				sameCount = 0
				last = ""
				klog.V(MyV).InfoS(msg(label, "attempted activation but none selected; resetting backoff"))
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
