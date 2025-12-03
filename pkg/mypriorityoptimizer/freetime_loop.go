// pkg/mypriorityoptimizer/freetime_loop.go
// freetime_loop.go

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// freeTimeLoop runs optimization only when the pending queue has been stable
// (same set of Pending pods) for at least OptimizeFreeTimeDelay.
//
// In async mode, the solver runs in the background and is *cancelled*
// if the pending set changes while it is running.
//
// Additionally, if we have already run the solver on a given pending set and
// it concluded optimally (applied a plan or reported no improvement / nothing
// schedulable), we will NOT run the solver again for that *exact* pending set.
// Only a change in the pending set will allow another run.
func (pl *SharedState) freeTimeLoop(ctx context.Context) {
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
	var lastSolvedSet map[types.UID]struct{} // last pending set we solved "optimally"
	lastChange := time.Now()

	// State for an in-flight free-time run
	var (
		runCancel   context.CancelFunc
		runDone     chan bool              // true if that run is considered "solved"
		baselineSet map[types.UID]struct{} // pending set at run start
	)

	for {
		select {
		case <-ctx.Done():
			// Cancel any in-flight run before exiting
			if runCancel != nil {
				runCancel()
			}
			if runDone != nil {
				<-runDone
			}
			return

		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.V(MyV).InfoS(msg(label, InfoCachesNotWarmedUp))
				lastPendingSet = nil
				lastChange = time.Now()
				timer.Reset(checkInterval)
				continue
			}

			// If a plan is active, let it finish; we don't start or cancel runs here.
			if ap := pl.getActivePlan(); ap != nil {
				klog.V(MyV).InfoS(msg(label, InfoActivePlanInProgress))
				timer.Reset(checkInterval)
				continue
			}

			// Snapshot current pods
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

			// ---- If we have a run in flight, track finish/cancel conditions ----
			if runDone != nil {
				select {
				case solved := <-runDone:
					// Run finished (success, "no improvement", or ctx canceled)
					klog.V(MyV).InfoS(msg(label, "free-time run finished"),
						"solved", solved)

					if solved && baselineSet != nil {
						// Remember this pending set as "solved".
						lastSolvedSet = cloneUIDSet(baselineSet)
					}
					runDone = nil
					runCancel = nil
					baselineSet = nil

				default:
					// Still running: cancel if the pending set changed vs baseline.
					if !sameUIDSet(currentSet, baselineSet) {
						klog.V(MyV).InfoS(msg(label, "pending set changed; cancelling free-time run"),
							"pending", pendingCount)
						runCancel()
					}
					// Do not start a new run until this one is fully finished.
					timer.Reset(checkInterval)
					continue
				}
			}

			// ---- No run in progress from here on ----

			// If we previously solved this exact pending set "optimally",
			// skip re-running the solver until the set changes.
			if lastSolvedSet != nil && sameUIDSet(currentSet, lastSolvedSet) {
				klog.V(MyV).InfoS(msg(label, "pending set matches last solved set; skipping optimization"),
					"pending", pendingCount)
				timer.Reset(checkInterval)
				continue
			}

			// Nothing pending → nothing to optimize, reset baselines.
			if pendingCount == 0 {
				if lastPendingSet != nil || lastSolvedSet != nil {
					lastPendingSet = nil
					lastSolvedSet = nil
					lastChange = time.Now()
				}
				timer.Reset(checkInterval)
				continue
			}

			// If the set of pending pods changed (not just the count), reset idle timer.
			if !sameUIDSet(currentSet, lastPendingSet) {
				lastPendingSet = cloneUIDSet(currentSet)
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

			// We have a stable pending set for at least 'delay' → start a background run.
			klog.InfoS(msg(label, InfoCycleStarted),
				"pendingPods", pendingCount,
				"idleFor", idleFor)

			baselineSet = cloneUIDSet(currentSet)
			ctxRun, cancelRun := context.WithCancel(ctx)
			runCancel = cancelRun
			runDone = make(chan bool, 1)

			go func() {
				_, _, _, _, _, err := pl.runFlow(ctxRun, nil)

				// Treat these as "solved" cases:
				// - err == nil: plan applied successfully.
				// - ErrNoImprovingSolutionFromAnySolver: baseline already optimal / no better plan.
				// - ErrNoPendingPodsToSchedule: nothing schedulable under current constraints.
				solved := err == nil ||
					err == ErrNoImprovingSolutionFromAnySolver ||
					err == ErrNoPendingPodsToSchedule

				if err != nil &&
					err != context.Canceled &&
					err != ErrNoImprovingSolutionFromAnySolver &&
					err != ErrNoPendingPodsToSchedule {
					klog.V(MyV).InfoS(msg(label, "runFlow completed with error"),
						"err", err.Error())
				}

				runDone <- solved
			}()

			// After starting a run, wait for either:
			//  - the run to finish (handled above), or
			//  - the pending set to change (also handled above).
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

// cloneUIDSet shallow-copies a UID set (so we don't alias maps by accident).
func cloneUIDSet(in map[types.UID]struct{}) map[types.UID]struct{} {
	if in == nil {
		return nil
	}
	out := make(map[types.UID]struct{}, len(in))
	for uid := range in {
		out[uid] = struct{}{}
	}
	return out
}
