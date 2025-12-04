// periodic_loop.go

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// periodicLoop runs the optimization loop at a regular interval.
func (pl *SharedState) periodicLoop(ctx context.Context) {
	klog.InfoS("Loop configuration", "optimizationInterval", OptimizeInterval.String())

	strategy := strategyToString()
	firstDelay := OptimizeInitialDelay
	interval := OptimizeInterval

	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	klog.InfoS(msg(strategy, InfoCycleStartedFirstRun), "in", firstDelay)

	var lastSolvedSet map[types.UID]struct{}
	var lastSolvedFingerprint string

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			if !pl.CachesWarm.Load() {
				klog.InfoS(msg(strategy, InfoCachesNotWarmedUp), "nextIn", interval)
				timer.Reset(interval)
				continue
			}

			snap, err := pl.buildPendingSnapshot()
			if err != nil {
				klog.InfoS(msg(strategy, "buildPendingSnapshot failed"),
					"err", err, "nextIn", interval)
				timer.Reset(interval)
				continue
			}

			pendingCount := snap.PendingCount

			// If Python is configured to only optimize the N lowest priorities and
			// there are currently no Pending pods in that low-priority window,
			// skip this optimization cycle entirely.
			if SolverPythonEnabled && SolverPythonNumLowerPriorities > 0 {
				if !pendingHasLowPriorityTargets(snap.Pods) {
					klog.V(MyV).InfoS(
						msg(strategy, "skip periodic optimization: only higher-priority pending pods outside python lower-tier window"),
						"pending", pendingCount,
						"pythonNumLowerPriorities", SolverPythonNumLowerPriorities,
					)
					timer.Reset(interval)
					continue
				}
			}

			if pendingCount == 0 {
				if lastSolvedSet != nil || lastSolvedFingerprint != "" {
					lastSolvedSet = nil
					lastSolvedFingerprint = ""
				}
				klog.InfoS(msg(strategy, "no pending pods; skipping optimization"))
				timer.Reset(interval)
				continue
			}

			if lastSolvedSet != nil &&
				lastSolvedFingerprint != "" &&
				sameUIDSet(snap.PendingUIDs, lastSolvedSet) &&
				snap.Fingerprint == lastSolvedFingerprint {
				klog.V(MyV).InfoS(
					msg(strategy, "pending set + cluster fingerprint match last solved; skipping optimization"),
					"pending", pendingCount,
				)
				timer.Reset(interval)
				continue
			}

			klog.InfoS(msg(strategy, InfoCycleStarted), "pendingPods", pendingCount)

			// No single preemptor in periodic modes.
			_, _, _, bestAttempt, _, err := pl.runFlow(context.Background(), nil)

			solved := isRunSolvedForPendingSet(err, bestAttempt)

			if err != nil &&
				err != ErrNoImprovingSolutionFromAnySolver &&
				err != ErrNoPendingPodsToSchedule {
				klog.V(MyV).InfoS(
					msg(strategy, "runFlow completed with error"),
					"err", err.Error(),
				)
			}

			if solved {
				lastSolvedSet = cloneUIDSet(snap.PendingUIDs)
				lastSolvedFingerprint = snap.Fingerprint
			}

			timer.Reset(interval)
			klog.InfoS(msg(strategy, InfoCycleNextRun), "in", interval)
		}
	}
}
