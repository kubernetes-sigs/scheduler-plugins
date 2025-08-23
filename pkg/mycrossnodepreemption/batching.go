// batching.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	BatchSolveInterval = 60 * time.Second // periodic cohort solve
	BatchInitialDelay  = 15 * time.Second // small delay before first run
)

type BatchIngressMode int

const (
	BatchOff BatchIngressMode = iota
	BatchPreEnqueue
	BatchPostFilter
)

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

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	ticker := time.NewTicker(BatchSolveInterval)
	defer ticker.Stop()

	first := time.After(BatchInitialDelay) // fires once after after startup with a short delay

	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
			first = nil
			pl.runBatchCycle()
		case <-ticker.C:
			pl.runBatchCycle()
		}
	}
}

func (pl *MyCrossNodePreemption) runBatchCycle() {
	// Bail if a plan is already running
	if sp, _ := pl.getActivePlan(); sp != nil && !sp.Completed {
		klog.InfoS("Batch loop: active plan in progress; skipping batch")
		return
	}

	// Remove stale entries from blocked and batch lists
	_ = pl.pruneBlockedStale()
	_ = pl.pruneBatchStale()

	pods := pl.snapshotBatch()
	if len(pods) == 0 {
		return
	}

	klog.InfoS("Batch loop: solving for batch", "batchSize", len(pods))

	// Run solver
	cctx, cancel := context.WithTimeout(context.Background(), PythonSolverTimeout)
	startTime := time.Now()
	out, err := pl.runPythonOptimizerCohort(cctx, pods, PythonSolverTimeout)
	cancel()
	if err != nil || out == nil {
		if err != nil {
			klog.ErrorS(err, "batch solve failed")
		}
		// Keep batch; try again next tick.
		return
	}

	// TODO: Not necessary "pending := pods[0]". Use first pod as "lead" (metadata only). No TargetNode requirement in batch.
	pending := pods[0]
	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil || plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		if err != nil {
			klog.ErrorS(err, "translate plan failed")
		}
		// If nothing to place, keep batch; try again next tick.
		if len(out.Placements) == 0 {
			return
		}
		plan = &PodAssignmentPlan{TargetNode: ""}
	}

	// Export plan for reference
	cmName, err := pl.exportPlanToConfigMap(context.Background(), plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "export plan failed")
	}

	lite, byName, wkDesired, err := pl.materializePlanDocs(plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "materialize plan docs failed")
		return
	}

	inMem := &StoredPlan{
		Completed:        false,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode, // TODO: may be empty for batch
		SolverOutput:     out,
		Plan:             lite,
		PlacementsByName: byName,
		WkDesiredPerNode: wkDesired,
	}
	pl.setActivePlan(inMem, cmName)

	klog.InfoS("batch solve completed", "status", out.Status, "duration", time.Since(startTime))

	_, planID := pl.getActivePlan()

	// TTL watchdog
	pl.startPlanTimeout(planID, PlanExecutionTTL)

	// Execute plan
	if err := pl.executePlan(context.Background(), plan); err != nil {
		klog.ErrorS(err, "plan execution failed")
		pl.onPlanSettled()
		return
	}

	// Activate batched pods, so they can be scheduled according to plan.
	// TODO: Move to seperate function
	activate := map[string]*v1.Pod{}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	for _, p := range pods {
		lp, err := podLister.Pods(p.Namespace).Get(p.Name)
		if err == nil {
			activate[p.Namespace+"/"+p.Name] = lp
		}
	}
	if len(activate) > 0 {
		pl.Handle.Activate(klog.Background(), activate)
	}

	// Batched processed; remove these pods from batch.
	pl.removePodsFromBatch(pods)

	// Count new and unscheduled from batch
	newFromBatch, unscheduledFromBatch := pl.countNewAndUnscheduledFromBatch(out.Placements, pods)

	klog.InfoS("batch plan executed; batch activated",
		"batchSize", len(pods),
		"planID", planID,
		"moved", len(plan.PodMovements),
		"evicted", len(plan.VictimsToEvict),
		"newFromBatch", newFromBatch,
		"unscheduledFromBatch", unscheduledFromBatch,
	)
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
