// opt_helpers.go

// mode_helpers.go (opt_helpers.go)

package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// startLoops launches background loops exactly once, after caches are warm.
// It is safe to call multiple times; only the first call does anything.
func (pl *SharedState) startLoops(ctx context.Context) {
	if !pl.CachesWarm.Load() {
		return
	}

	switch OptimizeMode {
	case ModePeriodic:
		go pl.modePeriodic(ctx)
	case ModeInterlude:
		go pl.modeInterlude(ctx)
	default:
		// Every@PreEnqueue uses the nudgeBlockedLoop helper.
		if optimizeEvery() && optimizeAtPreEnqueue() {
			go pl.nudgeBlockedLoop(ctx)
		}
	}
}

type optimizeLoopConfig struct {
	Label          string        // log label
	Interval       time.Duration // base tick interval
	InterludeDelay time.Duration // 0 => no "idle window"; >0 => require this long of stability
	CancelOnChange bool          // cancel in-flight run if pending set changes
}

func (pl *SharedState) optimizeBackgroundLoop(ctx context.Context, cfg optimizeLoopConfig) {
	strategy := strategyToString()

	interval := cfg.Interval
	if interval <= 0 {
		interval = 1 * time.Second
	}

	klog.InfoS(msg(cfg.Label, "started"),
		"mode", strategy,
		"interval", interval,
		"interludeeDelay", cfg.InterludeDelay,
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
			if !pl.CachesWarm.Load() {
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
			snap, err := pl.buildPendingSnapshot()
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
					if cfg.CancelOnChange && !sameUIDSet(currentSet, baselineSet) {
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
				sameUIDSet(currentSet, lastSolvedSet) &&
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
			if !sameUIDSet(currentSet, lastPendingSet) {
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

			// 9) Python lower-tier window
			if SolverPythonEnabled && SolverPythonNumLowerPriorities > 0 {
				if !pendingHasLowPriorityTargets(snap.Pods) {
					klog.V(MyV).InfoS(
						msg(cfg.Label, "skip run: only higher-priority pending pods outside python lower-tier window"),
						"pending", pendingCount,
						"pythonNumLowerPriorities", SolverPythonNumLowerPriorities,
					)
					timer.Reset(interval)
					continue
				}
			}

			// 10) Start a background run
			klog.InfoS(msg(cfg.Label, InfoCycleStarted),
				"pendingPods", pendingCount)

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
					klog.V(MyV).InfoS(msg(cfg.Label, "runFlow completed with error"),
						"err", err.Error())
				}

				runDone <- solved
			}()

			timer.Reset(interval)
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
