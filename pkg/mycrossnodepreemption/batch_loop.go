// batch_loop.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	firstDelay := OptimizationInitialDelay
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	klog.InfoS("Batch loop: first run scheduled", "in(s)", firstDelay)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			klog.InfoS("Batch loop: cycle")
			if _, err := pl.runBatchFlow(context.Background()); err != nil && err != ErrActiveInProgress {
				klog.ErrorS(err, "Batch loop: cycle failed")
			}
			timer.Reset(OptimizationInterval)
			klog.InfoS("Batch loop: next run", "in(s)", OptimizationInterval)
		}
	}
}

func (pl *MyCrossNodePreemption) runBatchFlow(ctx context.Context) (*BatchResult, error) {
	if !pl.tryEnterActive() {
		return nil, ErrActiveInProgress
	}
	done := func() { pl.leaveActive() }

	// Prune stale entries; keep only pending batched pods
	_ = pl.pruneStaleSetEntries(pl.Batched)

	pods := pl.snapshotBatch()
	if len(pods) == 0 {
		done()
		klog.InfoS("Batch loop: nothing to do")
		return &BatchResult{}, nil
	}

	start := time.Now()
	ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
	out, err := pl.solve(ctxSolve, SolveCohort, nil, pods, SolverTimeout)
	solverDur := time.Since(start)
	cancel()
	if err != nil || out == nil {
		done()
		if err == nil {
			err = ErrNoNomination // re-using sentinel for “no useful result”
		}
		klog.ErrorS(err, "Batch loop: solver failed")
		return nil, err
	}

	plan, ap, err := pl.registerPlan(ctx, out, nil)
	if err != nil {
		done()
		klog.ErrorS(err, "Batch loop: register plan failed")
		return nil, err
	}

	// Only execute when needed
	// Count which in cohort got a placement (for observability)
	newSched, stillUn := pl.countNewAndUnscheduledFromBatch(out.Placements, pods)
	if hasOps := pl.executePlanIfOps(ctx, plan); !hasOps {
		klog.InfoS("Batch loop: plan had no moves/evicts", "planID", ap.ID)
	}

	// activate everyone we just batched (so they re-enter queue)
	pl.activateBatchedPods(pods, 0)

	klog.InfoS("Batch loop: plan executed; waiting to settle",
		"planID", ap.ID,
		"batchSize", len(pods),
		"solverStatus", out.Status,
		"moves", len(plan.Moves),
		"evicts", len(plan.Evicts),
		"newScheduledFromBatch", newSched,
		"unscheduledFromBatch", stillUn,
		"batchDuration", time.Since(start),
		"solverDuration", solverDur,
	)

	// If nothing got newly scheduled we can complete immediately (no tail effects)
	if newSched == 0 {
		pl.onPlanSettled()
	}

	return &BatchResult{
		BatchSize:        len(pods),
		PlanID:           ap.ID,
		Moves:            len(plan.Moves),
		Evicts:           len(plan.Evicts),
		NewScheduled:     newSched,
		StillUnscheduled: stillUn,
		Status:           out.Status,
		TotalDuration:    time.Since(start),
		solverDuration:   solverDur,
	}, nil
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
