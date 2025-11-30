// plan_helpers.go

package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	clientv1 "k8s.io/client-go/listers/core/v1"
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

// buildPlan builds the evictions, movements, old placements, new placements, placementByName, workloadQuotas and the nominatedNode (if preemptor exists)
// from the output of the solver.
func (pl *SharedState) buildPlan(out *SolverOutput, preemptor *v1.Pod, pods []*v1.Pod) (*Plan, error) {
	if out == nil {
		return &Plan{}, nil
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
		WorkloadPerNodeCnts: buildWorkloadQuotasAtomics(plan.WorkloadQuotas),
		PlacementByName:     plan.PlacementByName, // we just pass PlacementsByName directly
		Ctx:                 ctxPlan,
		Cancel:              cancel,
	}

	// Store the new active plan
	pl.ActivePlan.Store(ap)

	// Activate watcher for plan timeout
	go pl.watchPlanTimeout(ap)
}

// buildWorkloadQuotasAtomics converts WorkloadQuotas (int32) to WorkloadPerNodeCnts (atomic.Int32)
// for faster concurrent access during plan execution.
func buildWorkloadQuotasAtomics(wkQuotas WorkloadQuotas) WorkloadQuotasAtomics {
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
			klog.V(MyV).InfoS("recreating standalone pod", "pod", podRef(pod))
			if err := pl.recreateStandalonePod(opCtx, pod, ""); err != nil {
				return fmt.Errorf("recreate %s: %w", podRef(pod), err)
			}
			return nil
		})
	}
	return g.Wait()
}

// waitTargetsGone waits until the evicted pods disappear from cache.
func (pl *SharedState) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	podsLister := pl.podsLister()

	// Poll until all pods are gone or context is done.
	return wait.PollUntilContextCancel(ctx, WaitPodsGoneInterval, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}
		for k := range remaining {
			p, err := podsLister.Pods(k.ns).Get(k.name)
			switch {
			case apierrors.IsNotFound(err):
				delete(remaining, k)
			case err != nil:
				// transient lister error; keep polling
				return false, nil
			default:
				// gone for our purposes if UID changed or deletion started
				if string(p.UID) != k.uid || p.DeletionTimestamp != nil {
					delete(remaining, k)
				}
			}
		}
		return len(remaining) == 0, nil
	})
}

// activatePlannedPending activates all live pending pods that the plan intends to place
// (i.e., NewPlacement with FromNode == "" and ToNode != "").
func (pl *SharedState) activatePlannedPending(plan *Plan, pods []*v1.Pod) {
	if plan == nil || len(plan.NewPlacements) == 0 || len(pods) == 0 {
		return
	}
	// Build allow-set of UIDs for pending -> scheduled in this plan.
	allow := make(map[types.UID]struct{}, len(plan.NewPlacements))
	for _, np := range plan.NewPlacements {
		if np.FromNode == "" && np.ToNode != "" {
			allow[np.Pod.UID] = struct{}{}
		}
	}
	if len(allow) == 0 {
		return
	}

	// Collect matching, truly-pending pods from the live slice.
	toAct := make(map[string]*v1.Pod, len(allow))
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil || p.Spec.NodeName != "" {
			continue // must be pending and alive
		}
		if _, ok := allow[p.UID]; !ok {
			continue
		}
		key := combineNsName(p.Namespace, p.Name)
		toAct[key] = p
	}
	if len(toAct) == 0 {
		return
	}
	pl.Handle.Activate(klog.Background(), toAct)
	klog.InfoS(InfoActivatingPlannedPendingPods, "count", len(toAct))
}

// resolvePod attempts to find the pod by matching UID and name,
// falling back to scanning all pods in the namespace to find a matching UID.
func (pl *SharedState) resolvePod(uid types.UID, ns, name string) *v1.Pod {
	podsLister := pl.podsLister()

	// Fast path: direct get matches UID
	if p, err := podsLister.Pods(ns).Get(name); err == nil && p != nil && p.UID == uid {
		return p
	}

	// Fallback: scan namespace for matching UID (handles renames or stale name → UID)
	if pods, err := podsLister.Pods(ns).List(labels.Everything()); err == nil {
		for _, p := range pods {
			if p.UID == uid {
				return p
			}
		}
	}
	return nil
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
func (pl *SharedState) isPlanCompleted(ctx context.Context, ap *ActivePlan, pod *v1.Pod) (bool, error) {
	if ap == nil {
		// Plan got down concurrently; treat as "not completed yet" (retry later)
		klog.V(MyV).InfoS("plan completion check skipped: no active plan doc")
		return false, nil
	}

	// Wait until the pod is visible+bound in cache when relevant.
	ok, err := pl.waitPendingBound(ctx, pod)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("timed out waiting for pending pod to be bound in cache")
	}

	podsLister := pl.podsLister()

	// A) Standalone/preemptor pods planned to specific nodes must be there.
	// Use the map from the active plan state: ns/name -> node.
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
			klog.V(MyV).InfoS("plan incomplete: pinned pod mismatch", "pod", nsname, "expectedNode", wantNode, "haveNode", po.Spec.NodeName)
			return false, nil
		}
	}

	// B) Per-workload per-node quotas consumed.
	for wk, perNode := range ap.WorkloadPerNodeCnts {
		for node, ctr := range perNode {
			if ctr.Load() > 0 {
				klog.V(MyV).InfoS("plan incomplete: remaining quota", "workload", wk, "node", node, "remaining", ctr.Load())
				return false, nil
			}
		}
	}

	return true, nil
}

// onPlanSettled is called when a plan is settled (i.e., all its actions are completed).
func (pl *SharedState) onPlanSettled(status PlanStatus) bool {
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
	// We do not activate blocked pods when we are in Every@PreEnqueue
	// as it would lead to high contention; instead we periodically nudge them.
	if !optimizeEvery() || !optimizeAtPreEnqueue() {
		pl.activatePods(pl.BlockedWhileActive, false, -1)
	}
	klog.InfoS(InfoDeactivatingActivePlan, "planID", ap.ID)

	// Mark the plan statuses in ConfigMaps
	pl.markPlanStatus(context.Background(), ap.ID, status)

	return true
}

// isPodAllowedByActivePlan returns true if the pod is allowed by the active plan.
// Standalone/preemptor pods are allowed by exact name match.
// For controller-owned pods, we allow only if the plan still has remaining
// per-node quota for that workload. If the pod already targets a specific
// node (NodeName set), we check that node's remaining quota; otherwise we
// allow if ANY node for that workload has remaining > 0.
func (pl *SharedState) isPodAllowedByActivePlan(pod *v1.Pod) bool {
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

// filterNodes returns the set of nodes the pod is allowed to run on according to the active plan.
func (pl *SharedState) filterNodes(pod *v1.Pod) (sets.Set[string], string, bool) {
	ap := pl.getActivePlan()
	if ap == nil {
		return nil, InfoNoActivePlan, true
	}

	// Standalone/preemptor addressed by name.
	if node, present := ap.PlacementByName[combineNsName(pod.Namespace, pod.Name)]; present {
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

	// Count pending that will be scheduled
	for _, plm := range out.Placements {
		if plm.ToNode == "" {
			continue
		}
		if p := pUID[plm.Pod.UID]; p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName == "" {
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

// waitPendingBound waits for the pending pod to be bound in the cache.
// By checking this for every pod we process in PostBind, we can be sure
// we have the correct state in cache before completing the plan.
func (pl *SharedState) waitPendingBound(ctx context.Context, pending *v1.Pod) (bool, error) {

	podsLister := pl.podsLister()
	namespace := pending.Namespace
	name := pending.Name
	uid := pending.UID
	podStr := combineNsName(namespace, name)

	// Single-shot check
	p, err := podsLister.Pods(namespace).Get(name)
	if err == nil && p.Spec.NodeName != "" {
		return true, nil
	}

	// Poll until the cached pod is bound.
	err = wait.PollUntilContextTimeout(ctx, PlanPendingBindInterval, PlanExecutionTimeout, true, func(ctx context.Context) (bool, error) {
		p, err := podsLister.Pods(namespace).Get(name)
		if apierrors.IsNotFound(err) { // pod not found; keep polling
			klog.V(MyV).InfoS("waiting for pending to appear in cache", "pod", podStr)
			return false, nil
		}
		if err != nil { // Lister error; keep polling
			klog.V(MyV).InfoS("lister error while waiting for pending", "pod", podStr, "err", err)
			return false, nil
		}
		if p.UID != uid { // waiting for matching pending UID
			klog.V(MyV).InfoS("waiting for matching pending UID", "pod", podStr, "wantUID", uid, "haveUID", p.UID)
			return false, nil
		}
		if p.Spec.NodeName == "" { // waiting for preemptor to bind
			klog.V(MyV).InfoS("waiting for pending to bind", "pod", podStr)
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

// exportPlanConfigMap exports the given plan to a ConfigMap.
func (pl *SharedState) exportPlanConfigMap(ctx context.Context, name string, sp *StoredPlan) error {
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
func (pl *SharedState) markPlanStatus(ctx context.Context, planCM string, status PlanStatus) {
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

// watchPlanTimeout monitors the timeout for the given active plan.
func (pl *SharedState) watchPlanTimeout(ap *ActivePlan) {
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
