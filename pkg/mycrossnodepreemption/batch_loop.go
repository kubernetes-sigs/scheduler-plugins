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
	_ = pl.pruneStaleSetEntries(pl.Blocked)
	_ = pl.pruneStaleSetEntries(pl.Batched)

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
	if len(keys) == 0 { // no pods in batch
		return nil
	}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister() // use SharedInformerFactory for immediate consistency
	snapshot := make([]*v1.Pod, 0, len(keys))                                  // preallocate slice
	for _, k := range keys {
		if pod, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil { // get current pod
			snapshot = append(snapshot, pod) // add pod to snapshot
		}
	}
	return snapshot
}
