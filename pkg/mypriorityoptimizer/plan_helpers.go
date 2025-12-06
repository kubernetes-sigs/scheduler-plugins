// plan_helpers.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	clientv1 "k8s.io/client-go/listers/core/v1"
)

// test hooks (overridden only in unit tests; nil in production).
var (
	evictTargetsHook           func(pl *SharedState, ctx context.Context, targets []*v1.Pod) error
	recreateStandalonePodsHook func(pl *SharedState, ctx context.Context, targets []*v1.Pod) error
	waitPodsGoneHook           func(pl *SharedState, ctx context.Context, pods []*v1.Pod) error
	activatePlannedPendingHook func(pl *SharedState, toActivate map[string]*v1.Pod)
	isPlanCompletedHook        func(pl *SharedState, ap *ActivePlan) (bool, error)
	onPlanCompletedHook        func(pl *SharedState, status PlanStatus, ap *ActivePlan)
	exportPlanToConfigMapHook  func(pl *SharedState, ctx context.Context, name string, sp *StoredPlan) error
	// If markPlanStatusToConfigMapHook returns true, the real implementation is skipped.
	markPlanStatusToConfigMapHook func(pl *SharedState, ctx context.Context, planCM string, status PlanStatus) bool
)

// tryEnterActive attempts to enter the active plan state.
// Use CompareAndSwap to ensure only one goroutine can enter the active state
// by checking that the previous value is false before setting it to true.
func (pl *SharedState) tryEnterActive() bool {
	return pl.Active.CompareAndSwap(false, true)
}

// leaveActive exits the active plan state.
func (pl *SharedState) leaveActive() {
	pl.Active.Store(false)
}

// getActivePlan returns the currently active plan, if any.
func (pl *SharedState) getActivePlan() *ActivePlan {
	return pl.ActivePlan.Load()
}

// clearActivePlan clears the currently active plan, if any.
func (pl *SharedState) tryClearActivePlan(ap *ActivePlan) bool {
	if ap == nil {
		return false
	}
	return pl.ActivePlan.CompareAndSwap(ap, nil)
}

// toPlanPod converts a core/v1 Pod into a Pod (UID, Namespace, Name).
func toPlanPod(p *v1.Pod) Pod {
	return Pod{
		UID:       p.UID,
		Namespace: p.Namespace,
		Name:      p.Name,
	}
}

// makePlacement builds a Placement for a pod on a given node.
func makePlacement(p *v1.Pod, node string) Placement {
	return Placement{
		Pod:  toPlanPod(p),
		Node: node,
	}
}

// makeNewPlacement builds a NewPlacement for a pod moving from src → dst.
func makeNewPlacement(p *v1.Pod, fromNode, toNode string) NewPlacement {
	return NewPlacement{
		Pod:      toPlanPod(p),
		FromNode: fromNode,
		ToNode:   toNode,
	}
}

// increaseWorkloadQuota increments the quota count for a workload/node pair.
func increaseWorkloadQuota(wq WorkloadQuotas, wk WorkloadKey, node string) {
	wkKey := wk.String()
	if wq[wkKey] == nil {
		wq[wkKey] = map[string]int32{}
	}
	wq[wkKey][node]++
}

// sortPlacementsByPod sorts placements by (namespace, name) for stable output.
func sortPlacementsByPod(pls []Placement) {
	sort.Slice(pls, func(i, j int) bool {
		pi, pj := pls[i].Pod, pls[j].Pod
		if pi.Namespace != pj.Namespace {
			return pi.Namespace < pj.Namespace
		}
		return pi.Name < pj.Name
	})
}

// sortNewPlacementsByPod sorts new placements by (namespace, name) for stable output.
func sortNewPlacementsByPod(pls []NewPlacement) {
	sort.Slice(pls, func(i, j int) bool {
		pi, pj := pls[i].Pod, pls[j].Pod
		if pi.Namespace != pj.Namespace {
			return pi.Namespace < pj.Namespace
		}
		return pi.Name < pj.Name
	})
}

// sortPodSetItemsByPriorityAndCreation sorts PodSetItems by:
//  1. priority (higher first)
//  2. creation timestamp (older first)
//  3. name (for zero/identical timestamps)
func sortPodSetItemsByPriorityAndCreation(items []PodSetItem) {
	sort.Slice(items, func(i, j int) bool {
		pi := getPodPriority(items[i].p)
		pj := getPodPriority(items[j].p)
		if pi != pj {
			return pi > pj
		}
		ti := items[i].p.GetCreationTimestamp().Time
		tj := items[j].p.GetCreationTimestamp().Time
		if ti.IsZero() || tj.IsZero() {
			// fallback: deterministic order by name if timestamps are missing
			return items[i].p.GetName() < items[j].p.GetName()
		}
		return ti.Before(tj)
	})
}

// isPlanPodUnscheduled returns true if the solver do not want pod to be placed
func isPlanPodUnscheduled(toNode string) bool {
	return toNode == ""
}

// isPlanPodMove returns true if the pod is currently bound to a node
// and the solver wants it on a different node.
func isPlanPodMove(fromNode, toNode string) bool {
	return fromNode != "" && fromNode != toNode
}

// isPlanPodPlacementChanged returns true if the pod's placement is changing
func isPlanPodPlacementChanged(fromNode, toNode string) bool {
	return fromNode == "" || fromNode != toNode
}

func isPlanPodNewlyScheduled(fromNode, toNode string) bool {
	return fromNode == "" && toNode != ""
}

// indexPodsForPlan builds a UID -> *Pod map for all live pods plus (optionally) the preemptor.
func indexPodsForPlan(pods []*v1.Pod, preemptor *v1.Pod) map[types.UID]*v1.Pod {
	byUID := podsByUID(pods)
	if preemptor != nil && !isPodDeleted(preemptor) {
		byUID[preemptor.UID] = preemptor
	}
	return byUID
}

// collectOldPlacements returns placements for all currently assigned & alive pods.
func collectOldPlacements(byUID map[types.UID]*v1.Pod) []Placement {
	oldPlacements := make([]Placement, 0, len(byUID))
	for _, p := range byUID {
		if isPodAssignedAndAlive(p) {
			oldPlacements = append(oldPlacements, makePlacement(p, getPodAssignedNodeName(p)))
		}
	}
	sortPlacementsByPod(oldPlacements)
	return oldPlacements
}

// collectEvictions builds evict placements from the solver output and pod index.
func collectEvictions(out *SolverOutput, byUID map[types.UID]*v1.Pod) []Placement {
	if out == nil || len(out.Evictions) == 0 {
		return nil
	}
	evicts := make([]Placement, 0, len(out.Evictions))
	for _, e := range out.Evictions {
		if p := byUID[e.Pod.UID]; isPodAssignedAndAlive(p) {
			evicts = append(evicts, makePlacement(p, getPodAssignedNodeName(p)))
		}
	}
	return evicts
}

// buildPlan builds the evictions, movements, old placements, new placements,
// placementByName, workloadQuotas and the nominatedNode (if preemptor exists)
// from the output of the solver.
func (pl *SharedState) buildPlan(out *SolverOutput, preemptor *v1.Pod, pods []*v1.Pod) (*Plan, error) {
	if out == nil {
		return &Plan{}, nil
	}

	// Index pods (alive only) + preemptor
	byUID := indexPodsForPlan(pods, preemptor)

	// Old placements (from current pod spec) and evictions (from solver).
	oldPlacements := collectOldPlacements(byUID)
	evicts := collectEvictions(out, byUID)

	var (
		moves         []NewPlacement
		newPlacements []NewPlacement
		nominatedNode string
	)

	placementByName := make(map[string]string)
	workloadQuotas := make(WorkloadQuotas)

	// Pass over placements to build:
	// - moves (running on a different node, non-preemptor)
	// - newPlacements (all)
	// - nominatedNode (preemptor)
	// - placementByName (standalone + preemptor)
	// - workloadQuotas (controller-owned, only when pending->node or node change)
	for _, plm := range out.Placements {
		if isPlanPodUnscheduled(plm.ToNode) {
			continue
		}

		p := byUID[plm.Pod.UID]
		if isPodDeleted(p) {
			continue
		}

		src := getPodAssignedNodeName(p)
		np := makeNewPlacement(p, src, plm.ToNode)
		newPlacements = append(newPlacements, np)

		moved := isPlanPodMove(src, plm.ToNode)

		// Always add preemptor to placementByName and set nominatedNode.
		if preemptor != nil && isSamePodUID(plm.Pod.UID, preemptor.UID) {
			placementByName[mergeNsName(p.Namespace, p.Name)] = plm.ToNode
			nominatedNode = plm.ToNode
			continue
		} else if moved {
			moves = append(moves, np)
		}
		if !isPlanPodPlacementChanged(src, plm.ToNode) {
			continue
		}

		// Controller-owned: go via WorkloadQuotas
		if wk, owned := topWorkload(p); owned {
			increaseWorkloadQuota(workloadQuotas, wk, plm.ToNode)
		} else {
			// Standalone pod: track directly by name.
			placementByName[mergeNsName(p.Namespace, p.Name)] = plm.ToNode
		}
	}

	// Stable ordering for new/moves.
	sortNewPlacementsByPod(newPlacements)
	sortNewPlacementsByPod(moves)

	return &Plan{
		Evicts:          evicts,
		Moves:           moves,
		OldPlacements:   oldPlacements,
		NewPlacements:   newPlacements,
		PlacementByName: placementByName,
		WorkloadQuotas:  workloadQuotas,
		NominatedNode:   nominatedNode,
	}, nil
}

// setActivePlan sets the given stored plan as the active plan and initializes its counters,
// deriving both WorkloadPerNodeCnts and PlacementByName solely from NewPlacements.
// For controller-owned pods, quotas are keyed by the controller (e.g., ReplicaSet) name.
func (pl *SharedState) setActivePlan(plan *Plan, id string, _ []*v1.Pod) {
	// Exit if no plan provided
	if plan == nil {
		klog.V(MyV).ErrorS(ErrNoPlanProvided, InfoNoPlanProvided+" in setActivePlan", nil)
		return
	}
	// Cancel any previous plan's timeout watcher.
	if old := pl.getActivePlan(); old != nil && old.Cancel != nil {
		old.Cancel()
	}

	// Build the new active plan state
	ctxPlan, cancel := context.WithTimeout(context.Background(), PlanExecutionTimeout)
	ap := &ActivePlan{
		ID:                  id,
		WorkloadPerNodeCnts: buildWorkloadQuotas(plan.WorkloadQuotas),
		PlacementByName:     plan.PlacementByName, // we just pass PlacementsByName directly
		Ctx:                 ctxPlan,
		Cancel:              cancel,
	}

	// Store the new active plan
	pl.ActivePlan.Store(ap)
}

// buildWorkloadQuotas converts WorkloadQuotas (int32) to WorkloadPerNodeCnts (atomic.Int32)
// for faster concurrent access during plan execution.
func buildWorkloadQuotas(wkQuotas WorkloadQuotas) WorkloadQuotasAtomics {
	remaining := make(WorkloadQuotasAtomics) // workload -> node -> *atomic.Int32
	if wkQuotas != nil {
		for wk, perNode := range wkQuotas {
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

// evictTargets evicts all target pods with bounded parallelism and per-op timeouts.
func (pl *SharedState) evictTargets(ctx context.Context, targets []*v1.Pod) error {
	if evictTargetsHook != nil {
		return evictTargetsHook(pl, ctx, targets)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(EvictParallelism)
	// Loop over targets and evict each in its own goroutine with timeout.
	for _, pod := range targets {
		g.Go(func() error {
			opCtx, cancel := context.WithTimeout(gctx, EvictTimeout)
			defer cancel()
			if err := pl.evictPod(opCtx, pod); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict %s: %w", podRef(pod), err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return nil
}

// recreateStandalonePods recreates only non-controller-owned pods (bounded parallelism).
func (pl *SharedState) recreateStandalonePods(ctx context.Context, targets []*v1.Pod) error {
	if recreateStandalonePodsHook != nil {
		return recreateStandalonePodsHook(pl, ctx, targets)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(RecreatePodParallelism)
	// Loop over targets and recreate each standalone pod in its own goroutine with timeout.
	for _, pod := range targets {
		// Skip controller-owned pods (their controllers will recreate them)
		if _, owned := topWorkload(pod); owned {
			continue
		}
		g.Go(func() error {
			opCtx, cancel := context.WithTimeout(gctx, RecreateTimeout)
			defer cancel()
			if err := pl.recreateStandalonePod(opCtx, pod, ""); err != nil {
				return fmt.Errorf("recreate %s: %w", podRef(pod), err)
			}
			return nil
		})
	}
	return g.Wait()
}

// waitTargetsGone waits until the evicted pods disappear from cache.
// waitPodsGone waits until the evicted pods disappear from cache.
func (pl *SharedState) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if waitPodsGoneHook != nil {
		return waitPodsGoneHook(pl, ctx, pods)
	}
	if len(pods) == 0 {
		return nil
	}
	// Use the shared Pod identity type (UID, Namespace, Name) as the key.
	remaining := make(map[Pod]struct{}, len(pods))
	for _, p := range pods {
		if p == nil {
			continue
		}
		remaining[toPlanPod(p)] = struct{}{}
	}
	// Poll until all pods are gone or context is done.
	return wait.PollUntilContextCancel(ctx, WaitPodsGoneInterval, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}
		for key := range remaining {
			p, err := pl.getPodByName(key.Namespace, key.Name)
			switch {
			case apierrors.IsNotFound(err):
				// Pod gone from the API/lister.
				delete(remaining, key)
			case err != nil:
				// Transient lister error; keep polling.
				return false, nil
			default:
				// Consider it "gone" for our purposes if the UID changed or it started deleting.
				if !isSamePodUID(p.UID, key.UID) || isPodDeleted(p) {
					delete(remaining, key)
				}
			}
		}
		return len(remaining) == 0, nil
	})
}

// activatePods performs the actual framework.Handle.Activate call.
// In tests we override this to capture which pods would be activated.
var activatePods = func(pl *SharedState, toAct map[string]*v1.Pod) {
	pl.Handle.Activate(klog.Background(), toAct)
}

// activateBlockedPods activates up to 'max' pods from the blocked set; clear only the ones activated.
// It returns the UIDs of the pods that were attempted to be activated (in priority/time order).
// if max <= 0, all pods are activated.
func (pl *SharedState) activatePods(podSet *PodSet, removeActivated bool, max int) (tried []types.UID) {
	// Prune stale entries first
	_ = pl.pruneSet(podSet)

	// If no blocked pods, nothing to do
	if !doesPodSetExist(podSet) {
		return
	}

	// Snapshot and resolve current Pod objects
	blockedPods := podSet.Snapshot()
	items := make([]PodSetItem, 0, len(blockedPods))
	// Get current Pod objects so that we don't return stale/deleted ones.
	for _, k := range blockedPods {
		if p, err := pl.getPodByName(k.Namespace, k.Name); err == nil && p != nil {
			items = append(items, PodSetItem{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return
	}

	// Sort by priority, then creation timestamp (older first), then name
	sortPodSetItemsByPriorityAndCreation(items)

	// Limit the number of pods to activate
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}

	// Build activation map and record "tried" UIDs
	toAct := make(map[string]*v1.Pod, limit)
	for _, it := range items[:limit] {
		toAct[mergeNsName(it.p.Namespace, it.p.Name)] = it.p
		tried = append(tried, it.key.UID)
	}

	if len(toAct) > 0 {
		activatePods(pl, toAct)
		klog.InfoS("activated pods", "set", podSet.Name, "count", len(toAct))
		if removeActivated {
			// Remove only the ones we just activated
			for _, it := range items[:limit] {
				podSet.RemovePod(it.key.UID)
			}
		}
	}
	return tried
}

// activatePlannedPending activates all live pending pods that the plan intends to place
// (i.e., NewPlacement with FromNode == "" and ToNode != "").
func (pl *SharedState) activatePlannedPending(plan *Plan, pods []*v1.Pod) {
	if plan == nil || len(plan.NewPlacements) == 0 || len(pods) == 0 {
		klog.V(MyV).InfoS("activatePlannedPending: no plan or no new placements or no pods",
			"planNil", plan == nil, "newPlacementsLen", len(plan.NewPlacements), "podsLen", len(pods))
		return
	}
	// Build allow-set of UIDs for pending -> scheduled in this plan.
	allow := make(map[types.UID]struct{}, len(plan.NewPlacements))
	for _, np := range plan.NewPlacements {
		if isPlanPodNewlyScheduled(np.FromNode, np.ToNode) {
			allow[np.Pod.UID] = struct{}{}
		}
	}
	if len(allow) == 0 {
		klog.V(MyV).InfoS("activatePlannedPending: no new placements with FromNode == \"\" and ToNode != \"\"")
		return
	}
	// Collect matching, truly-pending pods from the live slice.
	toAct := make(map[string]*v1.Pod, len(allow))
	for _, p := range pods {
		if isPodDeleted(p) || isPodAssigned(p) {
			continue // must be pending and alive
		}
		if _, ok := allow[p.UID]; !ok {
			continue
		}
		key := mergeNsName(p.Namespace, p.Name)
		toAct[key] = p
	}
	if len(toAct) == 0 {
		klog.V(MyV).InfoS("activatePlannedPending: no matching pending pods found")
		return
	}
	klog.InfoS(InfoActivatingPlannedPendingPods, "count", len(toAct))

	// Test hook: let unit tests observe the activation set without requiring a real Handle.
	if activatePlannedPendingHook != nil {
		activatePlannedPendingHook(pl, toAct)
		return
	}

	activatePods(pl, toAct)
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
// It is based on the current active plan snapshot (ap):
//
//	A) all pinned pods (PlacementByName) that still exist must run on the planned node;
//	   if a pinned pod was deleted or is terminating, we treat it as "no longer required".
//	B) all per-workload per-node quotas must be consumed, except for workloads that
//	   have been scaled down / deleted (no live pods) or have no pending pods left.
func (pl *SharedState) isPlanCompleted(ap *ActivePlan) (bool, error) {
	if isPlanCompletedHook != nil {
		return isPlanCompletedHook(pl, ap)
	}
	if ap == nil {
		// Plan got torn down concurrently; treat as "not completed yet" (retry later).
		klog.V(MyV).InfoS("plan completion check skipped: no active plan doc")
		return false, nil
	}
	workloadStatus := make(map[string]wkStatus)
	allPods, err := pl.getPods()
	if err != nil {
		return false, err
	}
	for _, p := range allPods {
		if isPodDeleted(p) {
			continue
		}
		if wk, owned := topWorkload(p); owned {
			key := wk.String()
			st := workloadStatus[key]
			st.hasLive = true
			if !isPodAssigned(p) {
				st.hasPending = true
			}
			workloadStatus[key] = st
		}
	}

	// A) Standalone/preemptor pods pinned by name must be on the expected nodes.
	//    If a pinned pod was deleted (or is terminating), we treat it as "satisfied"
	//    under the current workload (e.g., user scaled down or deleted it).
	for nsname, wantNode := range ap.PlacementByName {
		ns, name, err := splitNsName(nsname)
		if err != nil {
			return false, err
		}
		po, err := pl.getPodByName(ns, name)
		if apierrors.IsNotFound(err) {
			// The planned pod is gone (namespace/pod deleted or workload scaled down).
			// For completion purposes, we treat this as satisfied.
			klog.V(MyV).InfoS("plan completion: pinned pod gone; treating as satisfied",
				"pod", nsname,
				"expectedNode", wantNode,
			)
			continue
		}
		if err != nil {
			// Real lister error: don't claim completion yet, but retry later.
			return false, err
		}
		if isPodDeleted(po) {
			// Pod is terminating; we also treat this as "no longer required".
			klog.V(MyV).InfoS("plan completion: pinned pod terminating; treating as satisfied",
				"pod", nsname,
				"expectedNode", wantNode,
			)
			continue
		}
		haveNode := getPodAssignedNodeName(po)
		if isPlanPodPlacementChanged(haveNode, wantNode) {
			// Pod exists and is running on a different node than planned -> not complete.
			klog.V(MyV).InfoS("plan incomplete: pinned pod mismatch",
				"pod", nsname,
				"expectedNode", wantNode,
				"haveNode", haveNode,
			)
			return false, nil
		}
	}

	// B) Per-workload per-node quotas:
	// 	1) If total remaining == 0  -> satisfied.
	// 	2) If workload has NO live pods -> workload deleted/scale-to-zero -> ignore remaining.
	// 	3) If workload has pending pods -> still work to do -> not complete.
	// 	4) If workload has live but no pending pods -> scaled under plan / nothing left that can consume extra quota -> treat remaining quota as satisfied.
	for wk, perNode := range ap.WorkloadPerNodeCnts {
		var totalRemaining int32
		for _, ctr := range perNode {
			totalRemaining += ctr.Load()
		}

		// 1) All quota consumed -> satisfied
		if totalRemaining <= 0 {
			continue
		}

		st := workloadStatus[wk]

		// 2) Workload completely gone (no live pods): treat as satisfied.
		if !st.hasLive {
			klog.V(MyV).InfoS("plan completion: workload scaled down or deleted; ignoring remaining quota",
				"workload", wk,
				"remaining", totalRemaining,
			)
			continue
		}

		// 3) Workload still has pending pods and remaining quota: plan not done yet.
		if st.hasPending {
			klog.V(MyV).InfoS("plan incomplete: workload still has pending pods and remaining quota",
				"workload", wk,
				"remaining", totalRemaining,
			)
			return false, nil
		}

		// 4) Workload has live pods but no pending ones: we can't consume more quota,
		// so we treat the remaining quota as satisfied under the current workload.
		klog.V(MyV).InfoS("plan completion: workload has no pending pods; treating remaining quota as satisfied",
			"workload", wk,
			"remaining", totalRemaining,
		)
	}

	return true, nil
}

// onPlanCompleted is called when a plan is settled (i.e., all its actions are completed).
func (pl *SharedState) onPlanCompleted(status PlanStatus) bool {
	ap := pl.getActivePlan()
	// Win-or-lose: swap the ActivePlan pointer from 'ap' to nil.
	// Only the winner proceeds with teardown.
	if !pl.tryClearActivePlan(ap) {
		return false
	}

	// Winner zone: do the one-time teardown.
	pl.leaveActive() // flip Active=false
	if ap.Cancel != nil {
		ap.Cancel() // stop timeout watcher
	}

	// Allow tests to intercept teardown without hitting external deps.
	if onPlanCompletedHook != nil {
		onPlanCompletedHook(pl, status, ap)
		return true
	}

	// Activate blocked pods
	pl.activatePods(pl.BlockedWhileActive, false, -1)

	klog.InfoS(InfoDeactivatingActivePlan, "planID", ap.ID)

	// Mark the plan statuses in ConfigMaps
	pl.setPlanStatusInConfigMap(context.Background(), ap.ID, status)

	return true
}

// isPodAllowedByPlan returns true if the pod is allowed by the active plan.
// Standalone/preemptor pods are allowed by exact name match.
// For controller-owned pods, we allow only if the plan still has remaining
// per-node quota for that workload. If the pod already targets a specific
// node (NodeName set), we check that node's remaining quota; otherwise we
// allow if ANY node for that workload has remaining > 0.
func (pl *SharedState) isPodAllowedByPlan(pod *v1.Pod) bool {
	ap := pl.getActivePlan()
	if ap == nil {
		return false
	}

	// Standalone/preemptor pins addressed by name.
	if _, ok := ap.PlacementByName[mergeNsName(pod.Namespace, pod.Name)]; ok {
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
		if node := getPodAssignedNodeName(pod); node != "" {
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

// filterNodes returns the set of nodes the pod is allowed to run on according to the active plan.
func (pl *SharedState) filterNodes(pod *v1.Pod) (sets.Set[string], string, bool) {
	ap := pl.getActivePlan()
	if ap == nil {
		return nil, InfoNoActivePlan, true
	}

	// Standalone/preemptor addressed by name.
	if node, present := ap.PlacementByName[mergeNsName(pod.Namespace, pod.Name)]; present {
		if node != "" {
			return sets.New(node), "standalone; pin to planned node", true
		}
		return nil, "standalone; allowed by plan", true
	}

	// Controller-owned: enforce per-workload per-node quotas.
	if wk, owned := topWorkload(pod); owned {
		perNode := ap.WorkloadPerNodeCnts[wk.String()]
		if len(perNode) == 0 {
			return nil, "workload not in active plan; block", false
		}
		allowed := sets.New[string]()
		for node, ctr := range perNode {
			if ctr.Load() > 0 {
				allowed.Insert(node)
			}
		}
		if allowed.Len() == 0 {
			return nil, "workload quotas exhausted; block", false
		}
		return allowed, "workload nodes allowed", true
	}

	return nil, "pod not in active plan; block", false
}

// countNewAndTotalPods computes from the live cluster view and the solver output:
//
//	pendingScheduled = # of currently-pending pods that got a placement in this plan
//	totalPrePlan     = # of pods currently bound
//	totalPostPlan    = runningNow - evicted + pendingScheduled
func (pl *SharedState) countNewAndTotalPods(out *SolverOutput, pods []*v1.Pod) (pendingScheduled, totalPrePlan, totalPostPlan int) {
	if out == nil {
		return 0, 0, 0
	}
	totalPrePlan, pendingScheduled = 0, 0

	pUID := podsByUID(pods)
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if getPodAssignedNodeName(p) != "" {
			totalPrePlan++
		}
	}

	// Count evicted
	evicted := 0
	for _, e := range out.Evictions {
		if p := pUID[e.Pod.UID]; p != nil && !isPodDeleted(p) && getPodAssignedNodeName(p) != "" {
			evicted++
		}
	}

	// Count pending that will be scheduled
	for _, plm := range out.Placements {
		if plm.ToNode == "" {
			continue
		}
		if p := pUID[plm.Pod.UID]; p != nil && !isPodDeleted(p) && getPodAssignedNodeName(p) == "" {
			pendingScheduled++
		}
	}

	// Derive totalPostPlan
	totalPostPlan = totalPrePlan - evicted + pendingScheduled
	if totalPostPlan < 0 {
		totalPostPlan = 0
	}

	return pendingScheduled, totalPrePlan, totalPostPlan
}

// exportPlanToConfigMap exports the given plan to a ConfigMap.
func (pl *SharedState) exportPlanToConfigMap(ctx context.Context, name string, sp *StoredPlan) error {
	if exportPlanToConfigMapHook != nil {
		return exportPlanToConfigMapHook(pl, ctx, name, sp)
	}

	doc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      name,
		LabelKey:  PlanConfigMapLabelKey,
		DataKey:   PlanConfigMapLabelKey + ".json",
	}
	if err := doc.ensureJson(ctx, pl.Client.CoreV1(), sp); err != nil {
		return err
	}
	return pruneConfigMaps(ctx, pl.Client.CoreV1(), func(ns string) clientv1.ConfigMapNamespaceLister {
		return pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(ns)
	}, SystemNamespace, PlanConfigMapLabelKey, PlansToRetain)
}

// markPlanStatuses updates the plan's own ConfigMap and the exported stats CM.
//   - The plan CM is put into the requested status (unless already final).
//   - The exported stats CM only updates the last run if it isn't already final.
//     (Never overwrite Failed with Completed.)
func (pl *SharedState) setPlanStatusInConfigMap(ctx context.Context, planCM string, status PlanStatus) {
	if markPlanStatusToConfigMapHook != nil && markPlanStatusToConfigMapHook(pl, ctx, planCM, status) {
		return
	}

	lister := func(ns string) clientv1.ConfigMapNamespaceLister {
		return pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(ns)
	}
	planDoc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      planCM,
		LabelKey:  PlanConfigMapLabelKey,
		DataKey:   PlanConfigMapLabelKey + ".json",
	}
	_ = planDoc.mutateRaw(ctx, pl.Client.CoreV1(), lister, func(raw []byte) ([]byte, error) {
		var sp StoredPlan
		if err := json.Unmarshal(raw, &sp); err != nil {
			return nil, nil // best-effort
		}
		if sp.PlanStatus == PlanStatusCompleted || sp.PlanStatus == PlanStatusFailed {
			return nil, nil // final is sticky
		}
		sp.PlanStatus = status
		if status == PlanStatusCompleted || status == PlanStatusFailed {
			now := time.Now().UTC()
			sp.CompletedAt = &now
		}
		b, _ := json.MarshalIndent(&sp, "", "  ")
		return b, nil
	})
}

// clusterFingerprint builds a stable hash of the "cluster state" that matters
// for the solver baseline:
//
//   - all usable nodes (name + allocatable CPU/MEM)
//   - all RUNNING (non-terminating) pods bound to usable nodes
//     (UID + node + CPU/MEM + priority)
//
// Pending pods are explicitly *not* included here, since we track them via the
// pending UID set separately. We only use this to decide whether the cluster
// is "the same" baseline for a previously-solved pending set.
//
// The fingerprint is cheap to compute for small clusters and stable across
// map-iteration nondeterminism thanks to sorting.
func clusterFingerprint(nodes []*v1.Node, pods []*v1.Pod) string {
	h := fnv.New64a()

	// Filter usable nodes and sort them by name for determinism.
	usable := make([]*v1.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if isNodeUsable(n) {
			usable = append(usable, n)
		}
	}
	sort.Slice(usable, func(i, j int) bool {
		return usable[i].Name < usable[j].Name
	})

	// Node capacities.
	for _, n := range usable {
		cpu := getNodeCPUAllocatable(n)
		mem := getNodeMemoryAllocatable(n)
		_, _ = h.Write([]byte("N:"))
		_, _ = h.Write([]byte(n.Name))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(strconv.FormatInt(cpu, 10)))
		_, _ = h.Write([]byte("/"))
		_, _ = h.Write([]byte(strconv.FormatInt(mem, 10)))
		_, _ = h.Write([]byte(";"))
	}

	usableNames := make(map[string]struct{}, len(usable))
	for _, n := range usable {
		usableNames[n.Name] = struct{}{}
	}

	// Collect running pods on usable nodes and sort (node, UID) for determinism.
	type podKey struct {
		node string
		uid  types.UID
	}
	keys := make([]podKey, 0, len(pods))
	for _, p := range pods {
		if isPodDeleted(p) || !isPodAssigned(p) {
			continue
		}
		if _, ok := usableNames[getPodAssignedNodeName(p)]; !ok {
			continue
		}
		keys = append(keys, podKey{node: getPodAssignedNodeName(p), uid: p.UID})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].node != keys[j].node {
			return keys[i].node < keys[j].node
		}
		return keys[i].uid < keys[j].uid
	})

	byUID := podsByUID(pods)

	for _, k := range keys {
		p := byUID[k.uid]
		if p == nil {
			continue
		}
		cpu := getPodCPURequest(p)
		mem := getPodMemoryRequest(p)
		prio := getPodPriority(p)
		nodeName := getPodAssignedNodeName(p)

		_, _ = h.Write([]byte("P:"))
		_, _ = h.Write([]byte(string(p.UID)))
		_, _ = h.Write([]byte("@"))
		_, _ = h.Write([]byte(nodeName))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(strconv.FormatInt(cpu, 10)))
		_, _ = h.Write([]byte("/"))
		_, _ = h.Write([]byte(strconv.FormatInt(mem, 10)))
		_, _ = h.Write([]byte("#"))
		_, _ = h.Write([]byte(strconv.Itoa(int(prio))))
		_, _ = h.Write([]byte(";"))
	}

	return fmt.Sprintf("%x", h.Sum64())
}
