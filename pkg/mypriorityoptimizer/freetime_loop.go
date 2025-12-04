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
	var lastSolvedSet map[types.UID]struct{}
	var lastSolvedFingerprint string
	lastChange := time.Now()

	var (
		runCancel           context.CancelFunc
		runDone             chan bool
		baselineSet         map[types.UID]struct{}
		baselineFingerprint string
	)

	for {
		select {
		case <-ctx.Done():
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
				lastSolvedSet = nil
				lastSolvedFingerprint = ""
				lastChange = time.Now()
				timer.Reset(checkInterval)
				continue
			}

			if ap := pl.getActivePlan(); ap != nil {
				klog.V(MyV).InfoS(msg(label, InfoActivePlanInProgress))
				timer.Reset(checkInterval)
				continue
			}

			snap, err := pl.buildPendingSnapshot()
			if err != nil {
				klog.V(MyV).InfoS(msg(label, "buildPendingSnapshot failed"),
					"err", err)
				timer.Reset(checkInterval)
				continue
			}

			currentSet := snap.PendingUIDs
			pendingCount := snap.PendingCount
			currentFingerprint := snap.Fingerprint

			// Handle in-flight run
			if runDone != nil {
				select {
				case solved := <-runDone:
					klog.InfoS(msg(label, "free-time run finished"), "solved", solved)

					if solved && baselineSet != nil && baselineFingerprint != "" {
						lastSolvedSet = cloneUIDSet(baselineSet)
						lastSolvedFingerprint = baselineFingerprint
					}
					runDone = nil
					runCancel = nil
					baselineSet = nil
					baselineFingerprint = ""

				default:
					if !sameUIDSet(currentSet, baselineSet) {
						klog.V(MyV).InfoS(
							msg(label, "pending set changed; cancelling free-time run"),
							"pending", pendingCount,
						)
						runCancel()
					}
					timer.Reset(checkInterval)
					continue
				}
			}

			// No run in progress from here

			if lastSolvedSet != nil &&
				lastSolvedFingerprint != "" &&
				sameUIDSet(currentSet, lastSolvedSet) &&
				currentFingerprint == lastSolvedFingerprint {
				klog.InfoS(msg(label, "pending set + cluster fingerprint match last solved; skipping optimization"),
					"pending", pendingCount)
				timer.Reset(checkInterval)
				continue
			}

			if pendingCount == 0 {
				if lastPendingSet != nil || lastSolvedSet != nil || lastSolvedFingerprint != "" {
					lastPendingSet = nil
					lastSolvedSet = nil
					lastSolvedFingerprint = ""
					lastChange = time.Now()
				}
				timer.Reset(checkInterval)
				continue
			}

			if !sameUIDSet(currentSet, lastPendingSet) {
				lastPendingSet = cloneUIDSet(currentSet)
				lastChange = time.Now()
				klog.InfoS(msg(label, "pending set changed; reset idle timer"),
					"pending", pendingCount)
				timer.Reset(checkInterval)
				continue
			}

			idleFor := time.Since(lastChange)
			if idleFor < delay {
				timer.Reset(delay - idleFor)
				continue
			}

			if SolverPythonEnabled && SolverPythonNumLowerPriorities > 0 {
				if !pendingHasLowPriorityTargets(snap.Pods) {
					klog.InfoS(
						msg(label, "skip free-time run: only higher-priority pending pods outside python lower-tier window"),
						"pending", pendingCount,
						"pythonNumLowerPriorities", SolverPythonNumLowerPriorities,
						"idleFor", idleFor,
					)
					timer.Reset(checkInterval)
					continue
				}
			}

			klog.InfoS(msg(label, InfoCycleStarted), "pendingPods", pendingCount, "idleFor", idleFor)

			baselineSet = cloneUIDSet(currentSet)
			baselineFingerprint = currentFingerprint
			ctxRun, cancelRun := context.WithCancel(ctx)
			runCancel = cancelRun
			runDone = make(chan bool, 1)

			go func() {
				_, _, _, bestAttempt, _, err := pl.runFlow(ctxRun, nil)
				solved := isRunSolvedForPendingSet(err, bestAttempt)

				if err != nil &&
					err != context.Canceled &&
					err != ErrNoImprovingSolutionFromAnySolver &&
					err != ErrNoPendingPodsToSchedule {
					klog.V(MyV).InfoS(msg(label, "runFlow completed with error"),
						"err", err.Error())
				}

				runDone <- solved
			}()

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
