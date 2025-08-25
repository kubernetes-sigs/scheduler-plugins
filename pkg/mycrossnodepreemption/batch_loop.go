// batch_loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	firstDelay := BatchInitialDelay
	timer := time.NewTimer(firstDelay) // first run after initial delay
	nextAt := time.Now().Add(firstDelay)
	defer timer.Stop()

	klog.InfoS("Batch loop: first run scheduled", "in(s)", time.Until(nextAt).Round(time.Second))

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			klog.InfoS("Batch loop: starting cycle")
			pl.runBatchCycle()
			timer.Reset(BatchSolveInterval) // after first run, we use the regular interval
			klog.InfoS("Batch loop: next run scheduled", "in(s)", BatchSolveInterval)
		}
	}
}

func (pl *MyCrossNodePreemption) runBatchCycle() {
	// Check for active plan; skip batch if one is in progress
	if ap := pl.getActive(); ap != nil && !ap.PlanDoc.Completed {
		klog.InfoS("Batch loop: active plan in progress; skipping batch")
		return
	}

	// Prune stale entries; keep only pending pods
	_ = pl.pruneBlockedStale()
	_ = pl.pruneBatchStale()

	// Get current batch pods
	pods := pl.snapshotBatch()
	batchSize := len(pods)
	if batchSize == 0 {
		klog.InfoS("Batch loop: finished cycle; no pods to process")
		return
	}

	ctxSolve, cancel := context.WithTimeout(context.Background(), SolverTimeout)
	batchStart := time.Now()
	out, err := pl.solve(ctxSolve, SolveCohort, nil, pods, SolverTimeout)
	solverDuration := time.Since(batchStart)
	cancel()
	if err != nil || out == nil {
		klog.ErrorS(err, "Batch loop: batch solve failed")
		return
	}

	plan, ap, err := pl.publishPlan(context.Background(), out, nil)
	if err != nil {
		klog.ErrorS(err, "Batch loop: publish plan failed")
		return
	}

	// Skip plan execution if no moves or evictions
	newScheduledFromBatch, unsched := pl.countNewAndUnscheduledFromBatch(out.Placements, pods)
	if len(plan.Moves) > 0 || len(plan.Evicts) > 0 {
		if err := pl.executePlan(context.Background(), plan); err != nil {
			klog.ErrorS(err, "Batch loop: batch plan execution failed")
			pl.onPlanSettled()
			return
		}
	}

	pl.activateBatchedPods(pods)

	if len(plan.Moves) > 0 || len(plan.Evicts) > 0 || newScheduledFromBatch > 0 {
		klog.InfoS("Batch loop: finished cycle; waiting plan to settle",
			"solverStatus", out.Status,
			"batchSize", batchSize,
			"planID", ap.ID,
			"moves", len(plan.Moves),
			"evicts", len(plan.Evicts),
			"newScheduledFromBatch", newScheduledFromBatch,
			"unscheduledFromBatch", unsched,
			"batchDuration", time.Since(batchStart),
			"solverDuration", solverDuration,
		)
	} else {
		klog.InfoS("Batch loop: finished cycle with no changes")
		pl.onPlanSettled() // no changes, complete plan immediately
	}
}

func strategyEveryPreempter() bool    { return Strategy == StrategyEveryPreemptor }
func strategyBatchAtPreEnqueue() bool { return Strategy == StrategyBatchPreEnqueue }
func strategyBatchAtPostFilter() bool { return Strategy == StrategyBatchPostFilter }
func batchingEnabled() bool           { return Strategy != StrategyEveryPreemptor }

func strategyToString() string {
	switch Strategy {
	case StrategyEveryPreemptor:
		return "EveryPreemptor"
	case StrategyBatchPreEnqueue:
		return "BatchPreEnqueue"
	case StrategyBatchPostFilter:
		return "BatchPostFilter"
	default:
		return "Unknown"
	}
}

func (pl *MyCrossNodePreemption) countNewAndUnscheduledFromBatch(Placements map[string]string, pods []*v1.Pod) (int, int) {
	newFromBatch := 0
	unscheduledFromBatch := 0
	for _, p := range pods {
		if p == nil || p.Spec.NodeName != "" {
			// was already running; not part of "pending batch" accounting
			continue
		}
		node, ok := Placements[string(p.UID)]
		if ok && node != "" {
			newFromBatch++
		} else {
			unscheduledFromBatch++
		}
	}
	return newFromBatch, unscheduledFromBatch
}

func (pl *MyCrossNodePreemption) snapshotBatch() []*v1.Pod {
	keys := pl.Batched.Snapshot()
	if len(keys) == 0 {
		return nil
	}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	out := make([]*v1.Pod, 0, len(keys))
	for _, k := range keys {
		if cur, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil {
			out = append(out, cur)
		}
	}
	return out
}

func (pl *MyCrossNodePreemption) removePodsFromBatch(pods []*v1.Pod) {
	for _, p := range pods {
		pl.Batched.Remove(p.UID)
	}
}

func (pl *MyCrossNodePreemption) pruneBatchStale() int {
	rem := pl.pruneSetStale(pl.Batched, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep only pending pods
	})
	if rem > 0 {
		klog.V(2).InfoS("Pruned stale entries from batch", "removed", rem)
	}
	return rem
}

func (pl *MyCrossNodePreemption) activateBatchedPods(pods []*v1.Pod) {
	activate := map[string]*v1.Pod{}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	for _, p := range pods {
		if lp, err := podLister.Pods(p.Namespace).Get(p.Name); err == nil {
			activate[p.Namespace+"/"+p.Name] = lp
		}
	}
	if len(activate) > 0 {
		pl.Handle.Activate(klog.Background(), activate)
	}
	pl.removePodsFromBatch(pods)
}
