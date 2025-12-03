// periodic_loop.go

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// periodicLoop runs the optimization loop at a regular interval.
// It starts with an initial delay and then runs at a fixed interval.
// Additionally, if we have already run the solver on a given pending set and
// it concluded optimally (applied a plan or reported no improvement / nothing
// schedulable), we will NOT run the solver again for that *exact* pending set.
// Only a change in the pending set will allow another run.
func (pl *SharedState) periodicLoop(ctx context.Context) {
	klog.InfoS("Loop configuration", "optimizationInterval", OptimizeInterval.String())

	strategy := strategyToString()
	firstDelay := OptimizeInitialDelay
	interval := OptimizeInterval

	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	klog.InfoS(msg(strategy, InfoCycleStartedFirstRun), "in", firstDelay)

	// Last pending set on which we ran the solver and considered the result "solved".
	var lastSolvedSet map[types.UID]struct{}

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			// Defensive: normally startLoops() only starts this once caches are warm.
			if !pl.CachesWarm.Load() {
				klog.InfoS(msg(strategy, InfoCachesNotWarmedUp), "nextIn", interval)
				timer.Reset(interval)
				continue
			}

			// Snapshot pods and build current Pending set.
			pods, _ := pl.getPods()
			currentSet := make(map[types.UID]struct{})
			for _, p := range pods {
				if p == nil || p.Status.Phase != "Pending" {
					continue
				}
				currentSet[p.UID] = struct{}{}
			}
			pendingCount := len(currentSet)

			// No pending pods → nothing to optimize, clear solved baseline.
			if pendingCount == 0 {
				if lastSolvedSet != nil {
					lastSolvedSet = nil
				}
				klog.InfoS(msg(strategy, "no pending pods; skipping optimization"))
				timer.Reset(interval)
				continue
			}

			// If we previously solved this exact pending set, skip re-running the solver.
			if lastSolvedSet != nil && sameUIDSet(currentSet, lastSolvedSet) {
				klog.V(MyV).InfoS(
					msg(strategy, "pending set matches last solved set; skipping optimization"),
					"pending", pendingCount,
				)
				timer.Reset(interval)
				continue
			}

			klog.InfoS(msg(strategy, InfoCycleStarted),
				"pendingPods", pendingCount)

			// No single preemptor in periodic modes.
			_, _, _, _, _, err := pl.runFlow(context.Background(), nil)

			// Treat these outcomes as "solved" for this pending set:
			// - success (err == nil)
			// - no improving solution (baseline already optimal)
			// - nothing schedulable under current constraints.
			solved := err == nil ||
				err == ErrNoImprovingSolutionFromAnySolver ||
				err == ErrNoPendingPodsToSchedule

			if err != nil &&
				err != ErrNoImprovingSolutionFromAnySolver &&
				err != ErrNoPendingPodsToSchedule {
				klog.V(MyV).InfoS(
					msg(strategy, "runFlow completed with error"),
					"err", err.Error(),
				)
			}

			if solved {
				lastSolvedSet = cloneUIDSet(currentSet)
			}

			timer.Reset(interval)
			klog.InfoS(msg(strategy, InfoCycleNextRun), "in", interval)
		}
	}
}
