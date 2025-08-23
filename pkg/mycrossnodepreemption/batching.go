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
	// ======= Batch settings =======
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

	// Fires once after a short delay; doesn't block other program threads.
	first := time.After(BatchInitialDelay)

	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
			first = nil        // disable after it fires once
			pl.runBatchCycle() // your existing one-iteration function
		case <-ticker.C:
			pl.runBatchCycle()
		}
	}
}

// runBatchCycle executes one iteration of the batch solver path.
func (pl *MyCrossNodePreemption) runBatchCycle() {
	// bail if a plan is already running
	if sp, _ := pl.getActivePlan(); sp != nil && !sp.Completed {
		klog.InfoS("Batch loop: active plan in progress; skipping batch")
		return
	}

	// Ensure the cache has at least one usable node before touching the batch.
	if !pl.haveUsableNodes() {
		klog.InfoS("Batch loop: no usable nodes yet; keeping batch intact")
		return
	}

	_ = pl.pruneBlockedStale()
	_ = pl.pruneBatchStale()

	// Snapshot (do NOT clear yet). We only remove on success.
	pods := pl.snapshotBatch()
	if len(pods) == 0 {
		return
	}

	klog.InfoS("Batch loop: solving for batch", "batchSize", len(pods))

	// run cohort solve (no explicit preemptor)
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

	// Use the first pod as "lead" (metadata only). No TargetNode requirement in batch.
	pending := pods[0]

	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil || plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		if err != nil {
			klog.ErrorS(err, "translate plan failed")
		}
		// If there’s nothing to enforce at all, keep batch so we can retry later.
		if len(out.Placements) == 0 {
			return
		}
		plan = &PodAssignmentPlan{TargetNode: ""} // no imperative actions; just policy
	}

	// Persist + set active plan
	cmName, err := pl.exportPlanToConfigMap(context.Background(), plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "export plan failed; continuing in-memory")
	}

	lite, byName, wkDesired, err := pl.materializePlanDocs(plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "materialize plan docs failed")
		return
	}

	inMem := &StoredPlan{
		Completed:              false,
		GeneratedAt:            time.Now().UTC(),
		PluginVersion:          Version,
		PendingPod:             fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:             string(pending.UID),
		TargetNode:             plan.TargetNode, // may be empty for batch
		SolverOutput:           out,
		Plan:                   lite,
		PlacementsByName:       byName,
		WorkloadDesiredPerNode: wkDesired,
	}
	pl.setActivePlan(inMem, cmName)

	klog.InfoS("batch solve completed", "status", out.Status, "duration", time.Since(startTime))

	// TTL watchdog
	_, planID := pl.getActivePlan()
	pl.startPlanTimeout(planID, PlanExecutionTTL)

	// Execute imperative actions (may be none in pure batch-steering)
	if err := pl.executePlan(context.Background(), plan); err != nil {
		klog.ErrorS(err, "plan execution failed")
		pl.onPlanSettled()
		return
	}

	// Activate exactly the batch pods
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

	// Now that we succeeded, remove these pods from the batch.
	pl.removeFromBatchByUIDs(pods)

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
	// Count "new from batch" = pending pods in this batch that solver placed somewhere.
	// Also count "unscheduledFromBatch" = pending batch pods that got no placement
	// in an OPTIMAL/FEASIBLE solution (i.e., left out by optimality/constraints).
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
