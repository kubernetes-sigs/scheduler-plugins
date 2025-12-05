// loop_helpers.go

package mypriorityoptimizer

import (
	"context"
	"fmt"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// By default this just calls (*SharedState).optimizeGlobalBackgroundLoop.
// Tests can override this variable to intercept the cfg passed in.
var optimizeGlobalBackgroundLoopFunc = func(pl *SharedState, ctx context.Context, cfg optimizeLoopConfig) {
	pl.optimizeGlobalBackgroundLoop(ctx, cfg)
}

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

type optimizeLoopConfig struct {
	Label          string        // log label
	Interval       time.Duration // base tick interval
	InterludeDelay time.Duration // 0 => no "idle window"; >0 => require this long of stability
	CancelOnChange bool          // cancel in-flight run if pending set changes
}

func (pl *SharedState) optimizeGlobalBackgroundLoop(ctx context.Context, cfg optimizeLoopConfig) {
	strategy := modeToString()

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
				_, _, _, bestAttempt, _, err := pl.runOptimizationFlow(ctxRun, nil)
				solved := isAlreadySolvedForPendingSet(err, bestAttempt)

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

// isAlreadySolvedForPendingSet decides whether a run of runFlow has
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
func isAlreadySolvedForPendingSet(err error, bestAttempt *SolverResult) bool {
	if bestAttempt == nil {
		return false
	}
	if bestAttempt.Status != "OPTIMAL" {
		// Not a proven optimal solution → allow re-runs.
		return false
	}
	return err == ErrNoImprovingSolutionFromAnySolver ||
		err == ErrNoPendingPodsToSchedule
}

// pendingHasLowPriorityTargets reports whether, under the current
// SolverPythonNumLowerPriorities setting, there exists at least one
// Pending pod whose priority is within the "lowest N" distinct priorities
// observed in the cluster.
//
// Semantics:
//   - If SolverPythonNumLowerPriorities <= 0, we return true (no gating).
//   - If SolverPythonEnabled is false, we also return true (this gating is
//     only meaningful for the Python solver).
//   - Otherwise, we:
//     1) Collect all distinct priorities across *all* pods (running+pending)
//     2) Sort ascending and take the lowest N priorities
//     3) Check if any Pending pod has a priority in that set.
//     If none do, we return false → nothing for Python to do on this queue.
func pendingHasLowPriorityTargets(pods []*v1.Pod) bool {
	if !SolverPythonEnabled || SolverPythonNumLowerPriorities <= 0 {
		// Either Python is disabled or we're not restricting priorities:
		// from the Go side we should not skip runs based on priority windows.
		return true
	}

	// 1) Distinct priorities across all pods (running + pending)
	prioSet := make(map[int32]struct{})
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		pr := getPodPriority(p)
		prioSet[pr] = struct{}{}
	}
	if len(prioSet) == 0 {
		// No pods with a defined priority – be conservative and say "nothing to do".
		return false
	}

	// 2) Sorted ascending list of all priorities
	all := make([]int32, 0, len(prioSet))
	for pr := range prioSet {
		all = append(all, pr)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	// Keep only the N lowest distinct priorities
	limit := SolverPythonNumLowerPriorities
	if limit > len(all) {
		limit = len(all)
	}
	lowSet := make(map[int32]struct{}, limit)
	for i := 0; i < limit; i++ {
		lowSet[all[i]] = struct{}{}
	}

	// 3) Check if ANY Pending pod is in those lower priorities
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName != "" {
			continue // running, not in the pending queue
		}
		pr := getPodPriority(p)
		if _, ok := lowSet[pr]; ok {
			// At least one pending pod is in the low-priority window
			return true
		}
	}

	// Only higher-priority pending pods are present → Python won't touch them.
	return false
}

// PendingSnapshot bundles the pieces of state that both the periodic and
// free-time loops need in order to decide whether to run the solver.
type PendingSnapshot struct {
	PendingUIDs  map[types.UID]struct{}
	PendingCount int
	Fingerprint  string     // clusterFingerprint(nodes, pods)
	Pods         []*v1.Pod  // live snapshot (for priority checks)
	Nodes        []*v1.Node // live snapshot (for solver input)
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
