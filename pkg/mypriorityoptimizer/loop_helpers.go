// loop_helpers.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// By default this just calls (*SharedState).optimizeBackgroundLoop.
// Tests can override this variable to intercept the cfg passed in.
var optimizeBackgroundLoopFunc = func(pl *SharedState, ctx context.Context, cfg OptimizeLoopConfig) {
	pl.optimizeBackgroundLoop(ctx, cfg)
}

// test hooks – overridden only in unit tests.
var (
	buildPendingSnapshotHook = func(pl *SharedState) (*PendingSnapshot, error) {
		return pl.buildPendingSnapshot()
	}

	startBackgroundOptimization = func(
		pl *SharedState,
		cfg OptimizeLoopConfig,
		ctxRun context.Context,
		runDone chan<- bool,
	) {
		go func() {
			_, _, _, bestAttempt, _, err := pl.runOptimizationFlow(ctxRun, nil)
			solved := isAlreadyComputedForPendingSet(err, bestAttempt)

			if err != nil &&
				err != context.Canceled &&
				err != ErrNoImprovingSolutionFromAnySolver &&
				err != ErrNoPendingPodsScheduled {
				klog.V(MyV).InfoS(msg(cfg.Label, "runFlow completed with error"),
					"err", err.Error())
			}

			runDone <- solved
		}()
	}
)

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *SharedState) startLoops(ctx context.Context) {
	if !pl.PluginReady.Load() {
		return
	}
	switch OptimizeMode {
	case ModePeriodic:
		go pl.loopPeriodic(ctx)
	case ModeInterlude:
		go pl.loopInterlude(ctx)
	}
}

func (pl *SharedState) optimizeBackgroundLoop(ctx context.Context, cfg OptimizeLoopConfig) {
	strategy := getModeCombinedAsString()

	interval := cfg.Interval
	if interval <= 0 {
		interval = 1 * time.Second
	}

	klog.InfoS(msg(cfg.Label, "started"),
		"mode", strategy,
		"interval", interval,
		"interludeDelay", cfg.InterludeDelay,
		"cancelOnChange", cfg.CancelOnChange,
	)

	timer := time.NewTimer(interval)
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
			// 1) Cache warm-up
			if !pl.PluginReady.Load() {
				klog.V(MyV).InfoS(msg(cfg.Label, InfoCachesNotWarmedUp))
				lastPendingSet = nil
				lastSolvedSet = nil
				lastSolvedFingerprint = ""
				lastChange = time.Now()
				timer.Reset(interval)
				continue
			}

			// 2) If a plan is active, let it finish.
			if ap := pl.getActivePlan(); ap != nil {
				klog.V(MyV).InfoS(msg(cfg.Label, InfoActivePlanInProgress))
				timer.Reset(interval)
				continue
			}

			// 3) Snapshot
			snap, err := buildPendingSnapshotHook(pl)
			if err != nil {
				klog.V(MyV).InfoS(msg(cfg.Label, "buildPendingSnapshot failed"),
					"err", err)
				timer.Reset(interval)
				continue
			}

			currentSet := snap.PendingUIDs
			pendingCount := snap.PendingCount
			currentFingerprint := snap.Fingerprint

			// 4) In-flight run handling
			if runDone != nil {
				select {
				case solved := <-runDone:
					klog.InfoS(msg(cfg.Label, "background run finished"), "solved", solved)

					if solved && baselineSet != nil && baselineFingerprint != "" {
						lastSolvedSet = cloneUIDSet(baselineSet)
						lastSolvedFingerprint = baselineFingerprint
					}
					runDone = nil
					runCancel = nil
					baselineSet = nil
					baselineFingerprint = ""

				default:
					// Still running
					if cfg.CancelOnChange && !isSameUIDSet(currentSet, baselineSet) {
						klog.V(MyV).InfoS(
							msg(cfg.Label, "pending set changed; cancelling run"),
							"pending", pendingCount,
						)
						runCancel()
					}
					timer.Reset(interval)
					continue
				}
			}

			// 5) If we already solved exactly this set + fingerprint, skip.
			if lastSolvedSet != nil &&
				lastSolvedFingerprint != "" &&
				isSameUIDSet(currentSet, lastSolvedSet) &&
				currentFingerprint == lastSolvedFingerprint {
				klog.V(MyV).InfoS(
					msg(cfg.Label, "pending set + fingerprint match last solved; skipping optimization"),
					"pending", pendingCount,
				)
				timer.Reset(interval)
				continue
			}

			// 6) No pending → reset state
			if pendingCount == 0 {
				if lastPendingSet != nil || lastSolvedSet != nil || lastSolvedFingerprint != "" {
					lastPendingSet = nil
					lastSolvedSet = nil
					lastSolvedFingerprint = ""
					lastChange = time.Now()
				}
				timer.Reset(interval)
				continue
			}

			// 7) Track whether the pending set changed
			if !isSameUIDSet(currentSet, lastPendingSet) {
				lastPendingSet = cloneUIDSet(currentSet)
				lastChange = time.Now()
				klog.V(MyV).InfoS(
					msg(cfg.Label, "pending set changed; reset idle timer"),
					"pending", pendingCount,
				)
				timer.Reset(interval)
				continue
			}

			// 8) Free-time gating: require a stable window if FreeTimeDelay > 0
			if cfg.InterludeDelay > 0 {
				idleFor := time.Since(lastChange)
				if idleFor < cfg.InterludeDelay {
					timer.Reset(cfg.InterludeDelay - idleFor)
					continue
				}
			}

			// 9) Start a background run
			klog.InfoS(msg(cfg.Label, InfoCycleStarted),
				"pendingPods", pendingCount)

			baselineSet = cloneUIDSet(currentSet)
			baselineFingerprint = currentFingerprint

			ctxRun, cancelRun := context.WithCancel(ctx)
			runCancel = cancelRun
			runDone = make(chan bool, 1)

			startBackgroundOptimization(pl, cfg, ctxRun, runDone)

			timer.Reset(interval)
		}
	}
}

// isSameUIDSet returns true if a and b contain exactly the same UIDs.
func isSameUIDSet(a, b map[types.UID]struct{}) bool {
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

// isAlreadyComputedForPendingSet decides whether a run of runFlow has
// "fully solved" the current pending set, i.e. there is nothing
// better to do for this set of pending pods under the current cluster state.
// We only consider it solved when:
//   - runFlow returned ErrNoImprovingSolutionFromAnySolver OR
//     ErrNoPendingPodsToSchedule, AND
//   - bestAttempt is non-nil with Status == "OPTIMAL".
//
// If the solver only found a FEASIBLE solution, hit a time limit,
// was cancelled, or otherwise did not prove optimality, we return false
// so that the same pending set may be retried later.
func isAlreadyComputedForPendingSet(err error, bestAttempt *SolverResult) bool {
	if bestAttempt == nil {
		return false
	}
	if bestAttempt.Status != "OPTIMAL" {
		// Not a proven optimal solution → allow re-runs.
		return false
	}
	return err == ErrNoImprovingSolutionFromAnySolver ||
		err == ErrNoPendingPodsScheduled
}

// buildPendingSnapshot:
//   - lists current pods and nodes via informers
//   - builds the set of Pending pod UIDs
//   - computes the baseline cluster fingerprint (usable nodes + running pods).
func (pl *SharedState) buildPendingSnapshot() (*PendingSnapshot, error) {
	pods, err := pl.getPods()
	if err != nil {
		return nil, fmt.Errorf("buildPendingSnapshot: getPods failed: %w", err)
	}
	nodes, err := pl.getNodes()
	if err != nil {
		return nil, fmt.Errorf("buildPendingSnapshot: getNodes failed: %w", err)
	}

	pendingSet := make(map[types.UID]struct{})
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		// You currently check Status.Phase == "Pending" – use constant for clarity.
		if p.Status.Phase != v1.PodPending {
			continue
		}
		pendingSet[p.UID] = struct{}{}
	}

	fp := clusterFingerprint(nodes, pods)

	return &PendingSnapshot{
		PendingUIDs:  pendingSet,
		PendingCount: len(pendingSet),
		Fingerprint:  fp,
		Pods:         pods,
		Nodes:        nodes,
	}, nil
}
