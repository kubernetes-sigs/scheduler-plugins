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
func (pl *MyCrossNodePreemption) tryEnterActive() bool {
	return pl.Active.CompareAndSwap(false, true)
}

// leaveActive exits the active plan state.
func (pl *MyCrossNodePreemption) leaveActive() {
	pl.Active.Store(false)
}

// getActivePlan returns the currently active plan, if any.
func (pl *MyCrossNodePreemption) getActivePlan() *ActivePlan {
	return pl.ActivePlan.Load()
}

// clearActivePlan clears the currently active plan, if any.
func (pl *MyCrossNodePreemption) clearActivePlan() {
	pl.ActivePlan.Store(nil)
}

// registerPlan builds and registers a new plan as active, exporting it to a ConfigMap.
func (pl *MyCrossNodePreemption) registerPlan(
	ctx context.Context,
	solver SolverResult,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*Plan, *ActivePlan, string, error) {
	// Build the plan from the solver output
	plan, err := pl.buildPlan(solver.Output, preemptor, pods)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build actions: %w", err)
	}

	// Unique plan id (and ConfigMap name)
	id := fmt.Sprintf("plan-%d", time.Now().UnixNano())

	// Set active plan
	pl.setActivePlan(plan, id, pods)

	// Store the plan in a ConfigMap for debugging/inspection purposes
	storedPlan := &StoredPlan{
		PluginVersion:        Version,
		OptimizationStrategy: strategyToString(),
		GeneratedAt:          time.Now().UTC(),
		PlanStatus:           PlanStatusActive,
		Plan:                 plan,
		Solver:               summarizeAttempt(solver),
	}
	if preemptor != nil {
		storedPlan.Preemptor = &Preemptor{
			Pod: Pod{
				UID:       preemptor.UID,
				Namespace: preemptor.Namespace,
				Name:      preemptor.Name,
			},
			NominatedNode: plan.NominatedNode,
		}
	}
	if err := pl.exportPlanToConfigMap(ctx, id, storedPlan); err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	return plan, pl.getActivePlan(), plan.NominatedNode, nil
}

// executePlan executes the given plan: evicting and recreating pods as needed.
func (pl *MyCrossNodePreemption) executePlan(plan *Plan) error {
	// Defensive: nothing to do
	if plan == nil {
		klog.V(MyV).Info("executePlan: no plan provided; nothing to do")
		return nil
	}

	if len(plan.Moves) == 0 && len(plan.Evicts) == 0 {
		klog.V(MyV).Info("executePlan: plan has no moves or evictions; nothing to do")
		return nil
	}

	// Log plan details
	for _, mv := range plan.Moves {
		klog.V(MyV).InfoS("Pod movement planned",
			"pod", combineNsName(mv.Pod.Namespace, mv.Pod.Name), "from", mv.FromNode, "to", mv.ToNode)
	}
	for _, e := range plan.Evicts {
		klog.V(MyV).InfoS("Eviction planned",
			"pod", combineNsName(e.Pod.Namespace, e.Pod.Name), "from", e.Node)
	}

	ap := pl.getActivePlan()
	var baseCtx context.Context
	if ap != nil && ap.Ctx != nil {
		baseCtx = context.WithoutCancel(ap.Ctx)
	} else {
		baseCtx = context.Background()
	}
	overallCtx, cancel := context.WithTimeout(baseCtx, PlanOverallTimeout)
	defer cancel()

	// 1) Resolve unique targets (pods that are moved or evicted)
	seen := map[types.UID]bool{}
	var targets []*v1.Pod
	add := func(uid types.UID, ns, name string) {
		if seen[uid] {
			return
		}
		seen[uid] = true
		if pod := pl.resolvePod(uid, ns, name); pod != nil {
			targets = append(targets, pod)
		}
	}
	for _, mv := range plan.Moves {
		add(mv.Pod.UID, mv.Pod.Namespace, mv.Pod.Name)
	}
	for _, e := range plan.Evicts {
		add(e.Pod.UID, e.Pod.Namespace, e.Pod.Name)
	}

	// 2) Evict targets (bounded parallelism), then wait for them to disappear in cache
	if len(targets) > 0 {
		klog.V(MyV).InfoS("executePlan: evicting targeted pods", "count", len(targets))
		if err := pl.evictTargets(overallCtx, targets); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(overallCtx, WaitPodsGoneTimeout)
		defer cancel()
		if err := pl.waitPodsGone(waitCtx, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// 3) Recreate standalone (non-controller) pods only
	if err := pl.recreateStandalonePods(overallCtx, targets); err != nil {
		return err
	}

	return nil
}

// evictTargets evicts all target pods with bounded parallelism and per-op timeouts.
func (pl *MyCrossNodePreemption) evictTargets(ctx context.Context, targets []*v1.Pod) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(EvictParallelism) // bounded parallelism

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
func (pl *MyCrossNodePreemption) recreateStandalonePods(ctx context.Context, targets []*v1.Pod) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(RecreatePodParallelism) // bounded parallelism

	// Loop over targets and recreate each standalone pod in its own goroutine with timeout.
	for _, pod := range targets {
		// Skip controller-owned pods (their controllers will recreate them)
		if _, owned := topWorkload(pod); owned {
			continue
		}
		g.Go(func() error {
			opCtx, cancel := context.WithTimeout(gctx, RecreateTimeout)
			defer cancel()
			klog.V(MyV).InfoS("executePlan: recreating standalone pod", "pod", podRef(pod))
			if err := pl.recreateStandalonePod(opCtx, pod, ""); err != nil {
				return fmt.Errorf("recreate %s: %w", podRef(pod), err)
			}
			return nil
		})
	}
	return g.Wait()
}

// waitTargetsGone waits until the evicted pods disappear from cache.
func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
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

// resolvePod attempts to find the pod by matching UID and name,
// falling back to scanning all pods in the namespace to find a matching UID.
func (pl *MyCrossNodePreemption) resolvePod(uid types.UID, ns, name string) *v1.Pod {
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

// IsPodAllowedByActivePlan returns true if the pod is allowed by the active plan.
// Standalone/preemptor pods are allowed by exact name match.
// For controller-owned pods, we allow only if the plan still has remaining
// per-node quota for that workload. If the pod already targets a specific
// node (NodeName set), we check that node's remaining quota; otherwise we
// allow if ANY node for that workload has remaining > 0.
func (pl *MyCrossNodePreemption) IsPodAllowedByActivePlan(pod *v1.Pod) bool {
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
func (pl *MyCrossNodePreemption) exportPlanToConfigMap(ctx context.Context, name string, sp *StoredPlan) error {
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

// buildPlan builds the evictions, movements, old placements, new placements, placementByName, workloadQuotas and the nominatedNode (if preemptor exists)
// from the output of the solver.
func (pl *MyCrossNodePreemption) buildPlan(out *SolverOutput, preemptor *v1.Pod, pods []*v1.Pod) (*Plan, error) {
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

// waitPendingBound waits for the pending pod to be bound in the cache.
// By checking this for every pod we process in PostBind, we can be sure
// we have the correct state in cache before completing the plan.
func (pl *MyCrossNodePreemption) waitPendingBound(ctx context.Context, pending *v1.Pod) (bool, error) {

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
			klog.V(MyV).InfoS("Waiting for pending to appear in cache", "pod", podStr)
			return false, nil
		}
		if err != nil { // Lister error; keep polling
			klog.V(MyV).InfoS("Lister error while waiting for pending", "pod", podStr, "err", err)
			return false, nil
		}
		if p.UID != uid { // waiting for matching pending UID
			klog.V(MyV).InfoS("Waiting for matching pending UID", "pod", podStr, "wantUID", uid, "haveUID", p.UID)
			return false, nil
		}
		if p.Spec.NodeName == "" { // waiting for preemptor to bind
			klog.V(MyV).InfoS("Waiting for pending to bind", "pod", podStr)
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
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, ap *ActivePlan, pod *v1.Pod) (bool, error) {
	if ap == nil {
		// Plan got down concurrently; treat as "not completed yet" (retry later)
		klog.V(MyV).InfoS("Plan completion check skipped: no active plan doc")
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

// markPlanStatus sets the Status (Active/Completed/Failed) in the configMap.
// If the plan is already in a final state (Completed/Failed), it won't be downgraded.
func (pl *MyCrossNodePreemption) markPlanStatus(ctx context.Context, cmName string, status PlanStatus) error {
	doc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      cmName,
		LabelKey:  PlanConfigMapLabelKey,
		DataKey:   PlanConfigMapLabelKey + ".json",
	}
	// Load -> mutate -> patch
	raw, err := doc.readJson(func(ns string) clientv1.ConfigMapNamespaceLister {
		return pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(ns)
	})
	if err != nil || len(raw) == 0 {
		return err
	}
	var sp StoredPlan
	if json.Unmarshal(raw, &sp) != nil {
		return nil // best-effort
	}
	if sp.PlanStatus == PlanStatusCompleted || sp.PlanStatus == PlanStatusFailed {
		return nil // final is sticky
	}
	sp.PlanStatus = status
	if status == PlanStatusCompleted || status == PlanStatusFailed {
		now := time.Now().UTC()
		sp.CompletedAt = &now
	}
	return doc.patchJson(ctx, pl.Client.CoreV1(), &sp)
}

// TODO
// markExportedStatsPlanStatus sets/updates the last run's "plan_status"
// in the exported stats ConfigMap (namespace/name/key from solver_helpers.go).
// Rules mirror markPlanStatus: once in a final state (Completed or Failed) we
// don't change it; specifically we never overwrite Failed with Completed.
func (pl *MyCrossNodePreemption) markExportedStatsPlanStatus(ctx context.Context, status PlanStatus) error {
	doc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      SolverConfigMapExportedStatsName,
		LabelKey:  SolverConfigMapLabelKey,
		DataKey:   SolverConfigMapLabelKey + ".json",
	}
	// Replace with a manual read-modify-write sequence since MutateJSON is not defined.
	raw, err := doc.readJson(func(ns string) clientv1.ConfigMapNamespaceLister {
		return pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(ns)
	})
	if err != nil || len(raw) == 0 {
		return err
	}
	var runs []ExportedSolverStats
	if json.Unmarshal(raw, &runs) != nil {
		return nil // best-effort
	}
	if len(runs) > 0 {
		prev := runs[len(runs)-1].PlanStatus
		if prev != PlanStatusCompleted && prev != PlanStatusFailed {
			runs[len(runs)-1].PlanStatus = status
		}
	}
	return doc.patchJson(ctx, pl.Client.CoreV1(), &runs)
}

// TODO
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

// TODO
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

// TODO
// setActivePlan sets the given stored plan as the active plan and initializes its counters,
// deriving both WorkloadPerNodeCnts and PlacementByName solely from NewPlacements.
// For controller-owned pods, quotas are keyed by the controller (e.g., ReplicaSet) name.
func (pl *MyCrossNodePreemption) setActivePlan(plan *Plan, id string, _ []*v1.Pod) {
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

// TODO
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

// TODO
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
