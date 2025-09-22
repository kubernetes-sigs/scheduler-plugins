// plan_helpers.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// tryEnterActive attempts to enter the active plan state.
// Use CompareAndSwap to ensure only one goroutine can enter the active state
// by checking that the previous value is false before setting it to true.
func (pl *MyCrossNodePreemption) tryEnterActive() bool {
	return pl.Active.CompareAndSwap(false, true)
}

// leaveActive exits the active plan state.
func (pl *MyCrossNodePreemption) leaveActive() {
	pl.Active.Store(false)
}

// allowedByActivePlan returns true if the pod is allowed by the active plan.
// Standalone/preemptor pods are allowed by exact name match.
// For controller-owned pods, we allow only if the plan still has remaining
// per-node quota for that workload. If the pod already targets a specific
// node (NodeName set), we check that node's remaining quota; otherwise we
// allow if ANY node for that workload has remaining > 0.
func (pl *MyCrossNodePreemption) allowedByActivePlan(pod *v1.Pod) bool {
	ap := pl.getActivePlan()
	if ap == nil {
		return false
	}

	// Standalone/preemptor pins addressed by name.
	if _, ok := ap.PlacementByName[combineNsName(pod.Namespace, pod.Name)]; ok {
		return true
	}

	// Workload quotas (pods created by a controller).
	if wk, ok := topWorkload(pod); ok {
		perNode, ok := ap.WorkloadPerNodeCnts[wk.String()]
		if !ok || len(perNode) == 0 {
			return false
		}
		// If a node is already selected (rare at PreEnqueue, possible later),
		// require remaining quota on that specific node.
		if node := pod.Spec.NodeName; node != "" {
			if ctr, exists := perNode[node]; exists && ctr.Load() > 0 {
				return true
			}
			return false
		}
		// Otherwise, allow if ANY node still has remaining quota.
		for _, ctr := range perNode {
			if ctr.Load() > 0 {
				return true
			}
		}
		return false
	}

	return false
}

// onPlanSettled is called when a plan is settled (i.e., all its actions are completed).
func (pl *MyCrossNodePreemption) onPlanSettled(status PlanStatus) bool {
	ap := pl.getActivePlan()
	if ap == nil {
		return false
	}
	pl.clearActivePlan()
	pl.leaveActive()
	// We do not activate blocked pods when we are in Every@PreEnqueue
	// as it would lead to high contention; instead we periodically nudge them.
	if !optimizeEvery() || !optimizeAtPreEnqueue() {
		pl.activateBlockedPods(0)
	}
	if ap.Cancel != nil {
		ap.Cancel() // stop the timeout watcher
	}
	klog.InfoS("deactivating active plan", "planID", ap.ID)
	// Mark the plan status in ConfigMaps; ignore errors.
	_ = pl.markPlanStatus(context.Background(), ap.ID, status)
	_ = pl.markExportedStatsPlanStatus(context.Background(), status)
	return true
}

// exportPlanToConfigMap exports the given plan to a ConfigMap.
func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	name string,
	sp *StoredPlan,
) error {
	raw, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PlanConfigMapNamespace,
			Labels:    map[string]string{PlanConfigMapLabelKey: "true"},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.Client.CoreV1().ConfigMaps(PlanConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return err
	}
	_ = pl.pruneOldPlans(ctx, PlansToRetain)
	return nil
}

// TODO: Reach to here in this file...
// TODO: Reduce number of output parameters
// buildPlan builds the evictions, movements, old placements, new placements, placementByName, workloadQuotas and the nominatedNode (if preemptor exists)
// from the output of the solver.
func (pl *MyCrossNodePreemption) buildPlan(out *SolverOutput, preemptor *v1.Pod, pods []*v1.Pod) (*PlanBuild, error) {
	if out == nil {
		return &PlanBuild{}, nil
	}

	// Index pods by UID
	byUID := podsByUID(pods)
	if preemptor != nil {
		byUID[preemptor.UID] = preemptor
	}

	var (
		evicts        []Placement
		moves         []NewPlacement
		oldPlacements []Placement
		newPlacements []NewPlacement
		nominatedNode string
	)
	placementByName := make(map[string]string)
	workloadQuotas := make(WorkloadQuotas)

	// Old placements (all currently running)
	oldPlacements = oldPlacements[:0]
	for _, p := range byUID {
		if p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName != "" {
			oldPlacements = append(oldPlacements, Placement{
				Pod:  Pod{UID: p.UID, Namespace: p.Namespace, Name: p.Name},
				Node: p.Spec.NodeName,
			})
		}
	}
	sort.Slice(oldPlacements, func(i, j int) bool {
		if oldPlacements[i].Pod.Namespace != oldPlacements[j].Pod.Namespace {
			return oldPlacements[i].Pod.Namespace < oldPlacements[j].Pod.Namespace
		}
		return oldPlacements[i].Pod.Name < oldPlacements[j].Pod.Name
	})

	// Evictions (from solver output)
	for _, e := range out.Evictions {
		if p := byUID[e.Pod.UID]; p != nil && p.Spec.NodeName != "" {
			evicts = append(evicts, Placement{
				Pod:  Pod{UID: p.UID, Namespace: p.Namespace, Name: p.Name},
				Node: p.Spec.NodeName,
			})
		}
	}

	// Pass over placements to build:
	// - moves (running on a different node, non-preemptor)
	// - newPlacements (all)
	// - nominatedNode (preemptor)
	// - placementByName (standalone)
	// - workloadQuotas (controller-owned, only when pending→node or node change)
	for _, plm := range out.Placements {

		if plm.ToNode == "" {
			continue
		}
		p := byUID[plm.Pod.UID]
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}

		src := p.Spec.NodeName

		np := NewPlacement{
			Pod:      Pod{UID: p.UID, Namespace: p.Namespace, Name: p.Name},
			FromNode: src,
			ToNode:   plm.ToNode,
		}
		newPlacements = append(newPlacements, np)
		moved := src != "" && src != plm.ToNode

		// Always add preemptor to placementByName and set nominatedNode
		if preemptor != nil && IsPreemptor(plm.Pod.UID, preemptor.UID) {
			placementByName[combineNsName(p.Namespace, p.Name)] = plm.ToNode
			nominatedNode = plm.ToNode
			continue
		} else if moved {
			moves = append(moves, np)
		}

		change := src == "" || src != plm.ToNode
		if change {
			if wk, owned := topWorkload(p); owned {
				wkKey := wk.String()
				if workloadQuotas[wkKey] == nil {
					workloadQuotas[wkKey] = map[string]int32{}
				}
				workloadQuotas[wkKey][plm.ToNode] = workloadQuotas[wkKey][plm.ToNode] + 1
			} else {
				placementByName[combineNsName(p.Namespace, p.Name)] = plm.ToNode
			}
		}

	}
	// Stable ordering for new/moves
	sort.Slice(newPlacements, func(i, j int) bool {
		if newPlacements[i].Pod.Namespace != newPlacements[j].Pod.Namespace {
			return newPlacements[i].Pod.Namespace < newPlacements[j].Pod.Namespace
		}
		return newPlacements[i].Pod.Name < newPlacements[j].Pod.Name
	})
	sort.Slice(moves, func(i, j int) bool {
		if moves[i].Pod.Namespace != moves[j].Pod.Namespace {
			return moves[i].Pod.Namespace < moves[j].Pod.Namespace
		}
		return moves[i].Pod.Name < moves[j].Pod.Name
	})

	// Nil out empties for cleaner JSON
	if len(evicts) == 0 {
		evicts = nil
	}
	if len(moves) == 0 {
		moves = nil
	}
	if len(oldPlacements) == 0 {
		oldPlacements = nil
	}
	if len(newPlacements) == 0 {
		newPlacements = nil
	}
	if len(placementByName) == 0 {
		placementByName = nil
	}
	if len(workloadQuotas) == 0 {
		workloadQuotas = nil
	}

	return &PlanBuild{
		Evicts:          evicts,
		Moves:           moves,
		OldPlacements:   oldPlacements,
		NewPlacements:   newPlacements,
		PlacementByName: placementByName,
		WorkloadQuotas:  workloadQuotas,
		NominatedNode:   nominatedNode,
	}, nil
}

// pruneOldPlans removes old plans from the ConfigMap.
func (pl *MyCrossNodePreemption) pruneOldPlans(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	items, err := pl.listPlans(ctx)
	if err != nil {
		return err
	}
	if len(items) <= keep {
		return nil
	}
	latestIncomplete := ""
	for i := range items {
		raw := items[i].Data["plan.json"]
		if raw == "" {
			continue
		}
		var sp StoredPlan
		if json.Unmarshal([]byte(raw), &sp) == nil {
			if sp.Status != PlanStatusCompleted && sp.Status != PlanStatusFailed {
				latestIncomplete = items[i].Name
				break
			}
		}
	}
	keepSet := make(map[string]struct{}, keep)
	for i := 0; i < len(items) && len(keepSet) < keep; i++ {
		keepSet[items[i].Name] = struct{}{}
	}
	if latestIncomplete != "" {
		if _, ok := keepSet[latestIncomplete]; !ok {
			for i := keep - 1; i >= 0 && i < len(items); i-- {
				if _, ok := keepSet[items[i].Name]; ok && items[i].Name != latestIncomplete {
					delete(keepSet, items[i].Name)
					break
				}
			}
			keepSet[latestIncomplete] = struct{}{}
		}
	}
	for i := range items {
		name := items[i].Name
		if _, ok := keepSet[name]; ok {
			continue
		}
		if err := pl.Client.CoreV1().ConfigMaps(PlanConfigMapNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete old plan ConfigMap", "configMap", name)
		}
	}
	return nil
}

// waitPendingBoundInCache waits for the pending pod to be bound in the cache.
// By checking this for every pod we process in PostBind, we can be sure
// we have the correct state in cache before completing the plan.
func (pl *MyCrossNodePreemption) waitPendingBoundInCache(
	ctx context.Context,
	pending *v1.Pod,
) (bool, error) {
	podsLister := pl.podsLister()
	ns := pending.Namespace
	name := pending.Name
	// Single-shot check
	p, err := podsLister.Pods(ns).Get(name)
	if err == nil && p.Spec.NodeName != "" {
		return true, nil
	}
	// Poll until the cached pod is bound.
	err = wait.PollUntilContextTimeout(ctx, PlanPendingBindInterval, PlanExecutionTimeout, true, func(ctx context.Context) (bool, error) {
		p, err := podsLister.Pods(ns).Get(name)
		if apierrors.IsNotFound(err) { // pod not found; keep polling
			klog.V(MyV).InfoS("Waiting for pending to appear in cache", "pod", ns+"/"+name)
			return false, nil
		}
		if err != nil { // Lister error; keep polling
			klog.V(MyV).InfoS("Lister error while waiting for pending", "pod", ns+"/"+name, "err", err)
			return false, nil
		}
		if p.UID != pending.UID { // waiting for matching pending UID
			klog.V(MyV).InfoS("Waiting for matching pending UID", "pod", ns+"/"+name, "wantUID", pending.UID, "haveUID", p.UID)
			return false, nil
		}
		if p.Spec.NodeName == "" { // waiting for preemptor to bind
			klog.V(MyV).InfoS("Waiting for pending to bind", "pod", ns+"/"+name)
			return false, nil
		}
		return true, nil
	})
	if err == nil {
		return true, nil
	}
	// Timeout/cancel
	if errors.Is(err, context.DeadlineExceeded) {
		return false, nil
	}
	return false, err
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
// Mode: For-every: Single preemptor pod bound to target node (A) either in preenqueue or in postfilter, and all other pods in place (B, C)
// Mode: Batch: All pods bound to target nodes (only B, C).
// helpers.go
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, ap *ActivePlan, pod *v1.Pod) (bool, error) {
	if ap == nil {
		// Plan got torn down concurrently; treat as "not completed yet" (retry later)
		klog.V(MyV).InfoS("Plan completion check skipped: no active plan doc")
		return false, nil
	}

	// Wait until the pod is visible+bound in cache when relevant.
	ok, err := pl.waitPendingBoundInCache(ctx, pod)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("timed out waiting for pending pod to be bound in cache")
	}

	podsLister := pl.podsLister()

	// A) Standalone/preemptor pods planned to specific nodes must be there.
	// Use the hydrated map from the active plan state: ns/name -> node.
	for nsname, wantNode := range ap.PlacementByName {
		ns, name, err := splitNsName(nsname)
		if err != nil {
			return false, err
		}
		po, err := podsLister.Pods(ns).Get(name)
		if err != nil {
			return false, err
		}
		if po.DeletionTimestamp != nil || po.Spec.NodeName != wantNode {
			klog.V(MyV).InfoS("Plan incomplete: pinned pod mismatch", "pod", nsname, "expectedNode", wantNode, "haveNode", po.Spec.NodeName)
			return false, nil
		}
	}

	// B) Per-workload per-node quotas consumed.
	for wk, perNode := range ap.WorkloadPerNodeCnts {
		for node, ctr := range perNode {
			if ctr.Load() > 0 {
				klog.V(MyV).InfoS("Plan incomplete: remaining quota", "workload", wk, "node", node, "remaining", ctr.Load())
				return false, nil
			}
		}
	}

	return true, nil
}

// getActivePlan returns the currently active plan, if any.
func (pl *MyCrossNodePreemption) getActivePlan() *ActivePlan {
	return pl.ActivePlan.Load()
}

// clearActivePlan clears the currently active plan, if any.
func (pl *MyCrossNodePreemption) clearActivePlan() {
	pl.ActivePlan.Store(nil)
}

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(_ context.Context) ([]v1.ConfigMap, error) {
	sel := labels.SelectorFromSet(labels.Set{PlanConfigMapLabelKey: "true"})
	configMapLister := pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(PlanConfigMapNamespace)
	items, err := configMapLister.List(sel)
	if err != nil {
		return nil, err
	}
	out := make([]v1.ConfigMap, len(items))
	for i := range items {
		out[i] = *items[i].DeepCopy()
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreationTimestamp.Time.After(out[j].CreationTimestamp.Time)
	})
	return out, nil
}

// markPlanStatus sets the Status (Active/Completed/Failed) in the configMap.
// If the plan is already in a final state (Completed/Failed), it won't be downgraded.
func (pl *MyCrossNodePreemption) markPlanStatus(ctx context.Context, cmName string, status PlanStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		configMapLister := pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(PlanConfigMapNamespace)
		cm, err := configMapLister.Get(cmName)
		if apierrors.IsNotFound(err) || cm == nil {
			return nil
		}
		if err != nil {
			return err
		}
		raw := cm.Data["plan.json"]
		if raw == "" {
			return nil
		}
		var sp StoredPlan
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			klog.ErrorS(err, "markPlanStatus: cannot decode plan.json", "configMap", cmName)
			return nil
		}

		// If already final, don't overwrite with a different final status.
		if sp.Status == PlanStatusCompleted || sp.Status == PlanStatusFailed {
			return nil
		}

		// Set new status.
		sp.Status = status

		// If transitioning to a final state, stamp completion + duration (until all binds).
		if status == PlanStatusCompleted || status == PlanStatusFailed {
			now := time.Now().UTC()
			sp.CompletedAt = &now
		}

		b, _ := json.MarshalIndent(&sp, "", "  ")
		patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
		_, err = pl.Client.CoreV1().ConfigMaps(PlanConfigMapNamespace).
			Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}

// markExportedStatsPlanStatus sets/updates the last run's "plan_status"
// in the exported stats ConfigMap (namespace/name/key from solver_helpers.go).
// Rules mirror markPlanStatus: once in a final state (Completed or Failed) we
// don't change it; specifically we never overwrite Failed with Completed.
func (pl *MyCrossNodePreemption) markExportedStatsPlanStatus(ctx context.Context, status PlanStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cms := pl.Client.CoreV1().ConfigMaps(cmExportedStatsNamespace)
		cm, err := cms.Get(ctx, cmExportedStatsName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || cm == nil {
			return nil // nothing to do
		}
		if err != nil {
			return err
		}
		raw := cm.Data[cmExportedStatsKey]
		if raw == "" {
			return nil
		}
		var runs []ExportedStats
		if err := json.Unmarshal([]byte(raw), &runs); err != nil {
			klog.ErrorS(err, "markExportedStatsLastPlanStatus: cannot decode runs.json")
			return nil
		}
		if len(runs) == 0 {
			return nil
		}

		prev := runs[len(runs)-1].PlanStatus
		// Final is sticky. In particular, never set Completed if it was Failed.
		if prev == PlanStatusCompleted || prev == PlanStatusFailed {
			// already final; don't change
		} else {
			runs[len(runs)-1].PlanStatus = status
		}

		buf, _ := json.Marshal(runs)
		patch := []byte(fmt.Sprintf(`{"data":{"%s":%q}}`, cmExportedStatsKey, string(buf)))
		_, err = cms.Patch(ctx, cmExportedStatsName, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}

// watchPlanTimeout monitors the timeout for the given active plan.
func (pl *MyCrossNodePreemption) watchPlanTimeout(ap *ActivePlan) {
	<-ap.Ctx.Done()
	// If Cancel() was called due to completion/replacement, do nothing.
	if ap.Ctx.Err() != context.DeadlineExceeded {
		return
	}
	// Ensure we're still looking at the same active plan
	cur := pl.getActivePlan()
	if cur == nil || cur.ID != ap.ID {
		return
	}
	klog.InfoS("plan timeout reached; deactivating plan", "planID", ap.ID, "ttl", PlanExecutionTimeout)
	pl.onPlanSettled(PlanStatusFailed)
}

// buildWorkloadCntsAtomics converts WorkloadQuotas (int32) to WorkloadPerNodeCnts (atomic.Int32)
// for faster concurrent access during plan execution.
func buildWorkloadCntsAtomics(wkCnts WorkloadQuotas) WorkloadQuotasAtomics {
	remaining := make(WorkloadQuotasAtomics) // workload -> node -> *atomic.Int32
	if wkCnts != nil {
		for wk, perNode := range wkCnts {
			if remaining[wk] == nil {
				remaining[wk] = map[string]*atomic.Int32{}
			}
			for node, cnt := range perNode {
				if remaining[wk][node] == nil {
					remaining[wk][node] = new(atomic.Int32)
				}
				if cnt > 0 {
					remaining[wk][node].Store(cnt)
				}
			}
		}
		return remaining
	}
	return nil
}

// setActivePlan sets the given stored plan as the active plan and initializes its counters,
// deriving both WorkloadPerNodeCnts and PlacementByName solely from NewPlacements.
// For controller-owned pods, quotas are keyed by the controller (e.g., ReplicaSet) name.
func (pl *MyCrossNodePreemption) setActivePlan(plan *PlanBuild, id string, _ []*v1.Pod) {
	if plan == nil {
		return
	}

	workloadCnts := buildWorkloadCntsAtomics(plan.WorkloadQuotas)
	// Note: We just pass PlacementsByName directly

	// Cancel any previous plan's timeout watcher.
	if old := pl.getActivePlan(); old != nil && old.Cancel != nil {
		old.Cancel()
	}
	ctxPlan, cancel := context.WithTimeout(context.Background(), PlanExecutionTimeout)
	ap := &ActivePlan{
		ID:                  id,
		WorkloadPerNodeCnts: workloadCnts,
		PlacementByName:     plan.PlacementByName,
		Ctx:                 ctxPlan,
		Cancel:              cancel,
	}
	pl.ActivePlan.Store(ap)
	go pl.watchPlanTimeout(ap)
}

// allowedNodes returns:
// - node set to pin (non-nil) and Success, or
// - nil and an appropriate framework.Status reason to block/allow.
func (pl *MyCrossNodePreemption) allowedNodes(pod *v1.Pod) (sets.Set[string], string, bool) {
	ap := pl.getActivePlan()
	if ap == nil {
		return nil, "no active plan", true
	}

	// Standalone/preemptor by name
	if tgt, ok := ap.PlacementByName[combineNsName(pod.Namespace, pod.Name)]; ok && tgt != "" {
		return sets.New(tgt), "standalone; pin to planned node", true
	}

	// Workload quota routing
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		byNode, ok := ap.WorkloadPerNodeCnts[key]
		if !ok || len(byNode) == 0 {
			return nil, "workload not in active plan; block", false
		}
		nodesAllowed := sets.New[string]()
		for node, ctr := range byNode {
			if ctr.Load() > 0 {
				nodesAllowed.Insert(node)
			}
		}
		if nodesAllowed.Len() == 0 {
			return nil, "workload quotas exhausted; block", false
		}
		return nodesAllowed, "workload nodes allowed", true
	}

	return nil, "pod not in active plan; block", false
}

// countNewAndTotalPods computes from the live cluster view and the solver output:
//
//	pendingScheduled = # of currently-pending pods that got a placement in this plan
//	totalPrePlan     = # of pods currently bound
//	totalPostPlan    = runningNow - evicted + pendingScheduled
func (pl *MyCrossNodePreemption) countNewAndTotalPods(
	out *SolverOutput,
	live []*v1.Pod, // <-- pre-fetched once per flow
) (pendingScheduled, totalPrePlan, totalPostPlan int) {
	if out == nil {
		return 0, 0, 0
	}
	totalPrePlan, pendingScheduled = 0, 0

	pUID := podsByUID(live)
	for _, p := range live {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName != "" {
			totalPrePlan++
		}
	}

	// Count evicted
	evicted := 0
	for _, e := range out.Evictions {
		if p := pUID[e.Pod.UID]; p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName != "" {
			evicted++
		}
	}
	// Count pending that will be placed
	for _, plm := range out.Placements {
		if plm.ToNode == "" {
			continue
		}
		if p := pUID[plm.Pod.UID]; p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName == "" {
			pendingScheduled++
		}
	}
	totalPostPlan = totalPrePlan - evicted + pendingScheduled
	if totalPostPlan < 0 {
		totalPostPlan = 0
	}
	return pendingScheduled, totalPrePlan, totalPostPlan
}
