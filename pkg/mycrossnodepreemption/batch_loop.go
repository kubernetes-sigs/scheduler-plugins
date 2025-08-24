// batch_loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	// Schedule first run
	firstDelay := BatchInitialDelay
	tm := time.NewTimer(firstDelay)
	defer tm.Stop()

	nextAt := time.Now().Add(firstDelay)
	klog.InfoS("Batch loop: first run scheduled", "in", time.Until(nextAt).Round(time.Second))

	for {
		select {
		case <-ctx.Done():
			return

		case <-tm.C:
			start := time.Now()
			nextAt = time.Now().Add(BatchSolveInterval)
			klog.InfoS("Batch loop: starting cycle")
			batchSize, planID, moves, evicts, newScheduled, unsched := pl.runBatchCycle()
			if moves+evicts+newScheduled == 0 {
				klog.InfoS("Batch loop: finished cycle with no changes",
					"nextCycleIn(s)", time.Until(nextAt).Round(time.Second),
					"interval", BatchSolveInterval,
				)
			} else {
				klog.InfoS("Batch loop: finished cycle",
					"batchSize", batchSize,
					"planID", planID,
					"moves", moves,
					"evicts", evicts,
					"newScheduledFromBatch", newScheduled,
					"unscheduledFromBatch", unsched,
					"duration", time.Since(start),
					"nextCycleIn(s)", time.Until(nextAt).Round(time.Second),
					"interval", BatchSolveInterval,
				)
			}
			tm.Reset(BatchSolveInterval)
		}
	}
}

func (pl *MyCrossNodePreemption) runBatchCycle() (int, string, int, int, int, int) {
	if ap := pl.getActive(); ap != nil && !ap.PlanDoc.Completed {
		klog.InfoS("Batch loop: active plan in progress; skipping batch")
		return 0, "", 0, 0, 0, 0
	}
	// Prune stale entries; keep only pending pods
	_ = pl.pruneBlockedStale()
	_ = pl.pruneBatchStale()

	pods := pl.snapshotBatch()
	if len(pods) == 0 {
		return 0, "", 0, 0, 0, 0
	}

	ctxSolve, cancel := context.WithTimeout(context.Background(), SolverTimeout)
	out, err := pl.solve(ctxSolve, SolveCohort, nil, pods, SolverTimeout)
	cancel()
	if err != nil || out == nil {
		if err != nil {
			klog.ErrorS(err, "batch solve failed")
		}
		klog.InfoS("Batch loop: batch executed; no changes",
			"batchSize", len(pods),
		)
		return 0, "", 0, 0, 0, 0 // keep batch; try again next tick
	}

	// Use first pod as "lead" for metadata
	pending := pods[0]
	plan, ap, err := pl.publishPlan(context.Background(), out, pending)
	if err != nil {
		klog.ErrorS(err, "Batch loop: publish plan failed")
		return 0, "", 0, 0, 0, 0
	}

	// Skip plan execution if no moves or evictions
	if len(plan.Moves) > 0 || len(plan.Evicts) > 0 {
		if err := pl.executePlan(context.Background(), plan); err != nil {
			klog.ErrorS(err, "batch plan execution failed")
			pl.onPlanSettled()
			return 0, "", 0, 0, 0, 0
		}
	}

	pl.activateBatchedPods(pods)

	newScheduledFromBatch, unsched := pl.countNewAndUnscheduledFromBatch(out.Placements, pods)

	return len(pods), ap.ID, len(plan.Moves), len(plan.Evicts), newScheduledFromBatch, unsched
}

func batchAtPreEnqueue() bool { return BatchMode == BatchPreEnqueue }
func batchAtPostFilter() bool { return BatchMode == BatchPostFilter }
func batchingEnabled() bool   { return BatchMode != BatchOff }

func batchModeToString() string {
	switch BatchMode {
	case BatchOff:
		return "BatchOff"
	case BatchPreEnqueue:
		return "BatchPreEnqueue"
	case BatchPostFilter:
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
