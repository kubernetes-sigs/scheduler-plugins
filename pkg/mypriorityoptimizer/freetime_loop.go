package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// freetimeLoop runs optimization only when the pending queue has been stable
// (same set of Pending pods) for at least OptimizeFreeTimeDelay.
func (pl *SharedState) freetimeLoop(ctx context.Context) {
	label := "FreeTimeLoop"
	strategy := strategyToString()
	delay := OptimizeFreeTimeDelay
	checkInterval := OptimizeFreeTimeCheckInterval

	if delay <= 0 {
		delay = 2 * time.Second
	}
	if checkInterval <= 0 {
		checkInterval = delay / 4
		if checkInterval <= 0 {
			checkInterval = 250 * time.Millisecond
		}
	}

	klog.InfoS(msg(label, "started"),
		"mode", strategy,
		"freeTimeDelay", delay,
		"checkInterval", checkInterval,
	)

	timer := time.NewTimer(checkInterval)
	defer timer.Stop()

	var lastPendingSet map[types.UID]struct{}
	lastChange := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.V(MyV).InfoS(msg(label, InfoCachesNotWarmedUp))
				lastPendingSet = nil
				lastChange = time.Now()
				timer.Reset(checkInterval)
				continue
			}

			// If a plan is active, let it finish.
			if ap := pl.getActivePlan(); ap != nil {
				klog.V(MyV).InfoS(msg(label, InfoActivePlanInProgress))
				timer.Reset(checkInterval)
				continue
			}

			pods, _ := pl.getPods()

			// Build current set of Pending pod UIDs.
			currentSet := make(map[types.UID]struct{})
			for _, p := range pods {
				if p == nil || p.Status.Phase != "Pending" {
					continue
				}
				currentSet[p.UID] = struct{}{}
			}
			pendingCount := len(currentSet)

			// Nothing pending → nothing to optimize, reset baseline.
			if pendingCount == 0 {
				if lastPendingSet != nil {
					lastPendingSet = nil
					lastChange = time.Now()
				}
				timer.Reset(checkInterval)
				continue
			}

			// If the set of pending pods changed (not just the count), reset idle timer.
			if !sameUIDSet(currentSet, lastPendingSet) {
				lastPendingSet = currentSet
				lastChange = time.Now()
				klog.V(MyV).InfoS(msg(label, "pending set changed; reset idle timer"),
					"pending", pendingCount)
				timer.Reset(checkInterval)
				continue
			}

			// Same set of Pending pods; check how long they've been unchanged.
			idleFor := time.Since(lastChange)
			if idleFor < delay {
				// Wait the remaining time until we consider it "free time".
				timer.Reset(delay - idleFor)
				continue
			}

			// We have a stable pending set for at least 'delay' → run optimization.
			klog.InfoS(msg(label, InfoCycleStarted),
				"pendingPods", pendingCount,
				"idleFor", idleFor)

			_, _, _, _, _, err := pl.runFlow(context.Background(), nil)
			if err != nil {
				klog.V(MyV).InfoS(msg(label, "runFlow completed with error"),
					"err", err.Error())
			}

			// After a run, wait for the queue to change before running again.
			lastPendingSet = nil
			lastChange = time.Now()
			timer.Reset(checkInterval)
		}
	}
}

// sameUIDSet returns true if a and b contain exactly the same UIDs.
func sameUIDSet(a, b map[types.UID]struct{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for uid := range a {
		if _, ok := b[uid]; !ok {
			return false
		}
	}
	return true
}
