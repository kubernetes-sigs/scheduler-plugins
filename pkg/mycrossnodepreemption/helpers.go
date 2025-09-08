// helpers.go

package mycrossnodepreemption

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// ------ Batch Helpers -------

// snapshotBatch returns a snapshot of the current batch of pods.
func (pl *MyCrossNodePreemption) snapshotBatch() []*v1.Pod {
	keys := pl.Batched.Snapshot()
	if len(keys) == 0 { // no pods in batch
		return nil
	}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	snapshot := make([]*v1.Pod, 0, len(keys))
	for _, k := range keys {
		if pod, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil {
			snapshot = append(snapshot, pod)
		}
	}
	return snapshot
}

// ----- Strategy Helpers -------

// optimizeForEvery is the optimizer cadence that optimizes for every new pod.
func optimizeForEvery() bool { return OptimizeCadence == OptimizeForEvery }

// optimizeInBatches is the optimizer cadence that optimizes in batches.
func optimizeInBatches() bool { return OptimizeCadence == OptimizeInBatches }

// optimizeContinuously is the optimizer cadence that tries to optimize continuously if the cluster state hasn't changed during solver optimization.
func optimizeContinuously() bool { return OptimizeCadence == OptimizeContinuously }

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

// atPreEnqueue returns true if the phase is PreEnqueue.
func (phase Phase) atPreEnqueue() bool { return phase == PhasePreEnqueue }

// atPostFilter returns true if the phase is PostFilter.
func (phase Phase) atPostFilter() bool { return phase == PhasePostFilter }

// decideStrategy determines the optimization strategy based on the current phase.
// Continuously mode, never blocks or batches pods.
// Other modes, always block new pods, while actively optimizing.
// If not actively optimizing:
// OptimizeInBatches@PreEnqueue and OptimizeInBatches@PostFilter: batch new pods at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
// OptimizeForEvery@PreEnqueue and OptimizeForEvery@PostFilter: optimize for every new pod at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
func (pl *MyCrossNodePreemption) decideStrategy(phase Phase) StrategyDecision {
	// Mode: Continuously; never blocks or batches due to the optimizer.
	if optimizeContinuously() {
		return DecidePassThrough
	}
	// If not in continuous mode and there's an active plan, block all new pods.
	if pl.IsActivePlan() {
		return DecideBlockActive
	}
	// Modes: OptimizeInBatches@PreEnqueue or OptimizeInBatches@PostFilter
	if optimizeInBatches() {
		if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
			return DecideBatch // batch new pods
		}
		return DecidePassThrough // if not in the phase of optimization, allow all pods
	}
	// Modes: OptimizeForEvery@PreEnqueue or OptimizeForEvery@PostFilter
	if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
		return DecideEvery // optimize for every new pod
	}
	return DecidePassThrough // if not at the phase of optimization, allow all pods
}

// modeToString returns a string representation of the optimization mode.
func modeToString() string {
	a := "ForEvery"
	switch OptimizeCadence {
	case OptimizeInBatches:
		a = "InBatches"
	case OptimizeContinuously:
		a = "Continuous"
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// ---------- Cache Helpers ----------------

func (pl *MyCrossNodePreemption) WaitForInformersSynced(
	ctx context.Context,
	podsInf, nodesInf, cmsInf, rsInf, ssInf, dsInf, jobInf cache.SharedIndexInformer,
) {
	ok := cache.WaitForCacheSync(ctx.Done(),
		podsInf.HasSynced, nodesInf.HasSynced, cmsInf.HasSynced,
		rsInf.HasSynced, ssInf.HasSynced, dsInf.HasSynced, jobInf.HasSynced,
	)
	if !ok {
		klog.InfoS("Cache sync aborted (context done)")
		return
	}

	// Start a background watcher that waits for a usable node.
	// We immediately return so the caller can continue even if no usable nodes exist yet.
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				// Do NOT activate blocked pods here; only when a usable node is found.
				return

			case <-t.C:
				nodes, err := pl.getNodes()
				if err != nil {
					klog.V(V2).InfoS("Cache warm-up watcher: getNodes error; retrying", "err", err)
					continue
				}
				usable := false
				for _, n := range nodes {
					if isNodeUsable(n) {
						usable = true
						break
					}
				}
				if !usable {
					klog.V(V2).InfoS("Cache warm-up: waiting for a usable node")
					continue
				}

				// We have at least one usable node—mark warm and (optionally) unblock.
				pl.CachesWarm.Store(true)
				klog.InfoS("Caches ready and usable node(s) detected")

				// Avoid a surge in ForEvery@PreEnqueue; the idle nudge will trickle them.
				if !(optimizeForEvery() && optimizeAtPreEnqueue()) {
					_ = pl.activateBlockedPods(0) // activate all currently blocked
				}
				return
			}
		}
	}()

	// Return immediately; no usable nodes yet is fine — only unblock once watcher confirms usability.
}

// ---------- Objects Helpers --------------

// getNodes returns a list of all nodes in the cluster.
// Use the informer lister to avoid stale data from SnapshotLister.
func (pl *MyCrossNodePreemption) getNodes() ([]*v1.Node, error) {
	return pl.Handle.SharedInformerFactory().Core().V1().Nodes().Lister().List(labels.Everything())
}

// getPods returns a list of all pods in the cluster.
// Use the informer lister to avoid stale data from SnapshotLister.
func (pl *MyCrossNodePreemption) getPods() ([]*v1.Pod, error) {
	return pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister().List(labels.Everything())
}

// podRef returns a string representation of the pod's namespace and name.
func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func combineNsName(ns, name string) string {
	return fmt.Sprintf("%s/%s", ns, name)
}

// evictPod evicts a pod from the cluster using the eviction API.
func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0) // immediate eviction
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		},
	}
	return pl.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
}

// waitPodsGone waits for the specified pods to be deleted from the cluster.
func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}
	type key struct{ ns, name, uid string }
	rem := make(map[key]struct{}, len(pods))
	// populate the map with pod keys
	for _, p := range pods {
		rem[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if len(rem) == 0 { // all pods are gone
			return true, nil
		}
		for k := range rem {
			p, err := l.Pods(k.ns).Get(k.name)
			if apierrors.IsNotFound(err) { // pod is gone
				delete(rem, k)
				continue
			}
			if err != nil { // keep polling
				return false, nil
			}
			if string(p.UID) != k.uid || p.DeletionTimestamp != nil { // recreated or terminating
				delete(rem, k)
			}
		}
		return len(rem) == 0, nil
	})
}

// recreatePod creates a new pod with the same specifications as the original pod.
// Needed for standalone pods as when they are evicted, they will not be recreated as they have no controllers.
func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.UID = ""
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.NodeName = "" // no direct binding
	newp.Spec.NodeSelector = map[string]string{}
	if _, err := pl.Client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	return nil
}

// --------- Pod specifications Helpers ---------

// getPodCPURequest returns the total CPU request for a pod by summing the requests of all containers.
func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Cpu().MilliValue()
	}
	return total
}

// getPodMemoryRequest returns the total memory request for a pod by summing the requests of all containers.
func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
	}
	return total
}

// getPodPriority returns the priority of a pod.
func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}

// isControlPlane returns true if the node is a control plane node.
// Additional labels can be added here as needed.
func isControlPlane(n *v1.Node) bool {
	return n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
}

// nodeReady returns true if the node is ready.
func nodeReady(n *v1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

// hasNoScheduleCondTaint returns true if the node has a NoSchedule taint due to not ready or unreachable conditions.
func hasNoScheduleCondTaint(n *v1.Node) bool {
	for _, t := range n.Spec.Taints {
		if (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") &&
			(t.Effect == v1.TaintEffectNoSchedule || string(t.Effect) == "") {
			return true
		}
	}
	return false
}

// isNodeUsable returns true if the node is usable for scheduling.
func isNodeUsable(n *v1.Node) bool {
	if n == nil || isControlPlane(n) || n.Spec.Unschedulable {
		return false
	}
	if !nodeReady(n) {
		return false
	}
	if hasNoScheduleCondTaint(n) {
		return false
	}
	return n.Status.Allocatable.Cpu().MilliValue() > 0 &&
		n.Status.Allocatable.Memory().Value() > 0
}

// --------- Pod set Helpers ---------

// newPodSet creates a new PodSet.
func newPodSet() *PodSet { return &PodSet{m: make(map[types.UID]PodKey)} }

// AddPod adds a pod to the set.
func (s *PodSet) AddPod(p *v1.Pod) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.m[p.UID] = PodKey{UID: p.UID, Namespace: p.Namespace, Name: p.Name}
	s.mu.Unlock()
}

// RemovePod removes a pod from the set.
func (s *PodSet) RemovePod(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// Clear removes all pods from the set.
func (s *PodSet) Clear() {
	s.mu.Lock()
	s.m = make(map[types.UID]PodKey)
	s.mu.Unlock()
}

// Size returns the number of pods in the set.
func (s *PodSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

// Snapshot returns a snapshot of the current pods in the set.
func (s *PodSet) Snapshot() map[types.UID]PodKey {
	s.mu.RLock()
	out := make(map[types.UID]PodKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}

// pruneSetStale removes pods from the set that are no longer present.
func (pl *MyCrossNodePreemption) pruneSetStale(set *PodSet, keep func(cur *v1.Pod) bool) int {
	if set == nil || set.Size() == 0 {
		return 0
	}
	snap := set.Snapshot()
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	removed := 0
	for uid, key := range snap {
		cur, err := podLister.Pods(key.Namespace).Get(key.Name)
		switch {
		case apierrors.IsNotFound(err): // pod no longer exists; remove from set
			set.RemovePod(uid)
			removed++
		case err != nil:
			// Conservative; keep if lister errored
		default: // pod has been recreated/terminating; remove from set
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil {
				set.RemovePod(uid)
				removed++
				continue
			}
			// Apply caller-specific predicate
			if keep != nil && !keep(cur) {
				set.RemovePod(uid)
				removed++
			}
		}
	}
	return removed
}

// activateBlockedPods activates up to 'max' pods from the blocked set; clear only the ones activated.
// It returns the UIDs of the pods that were *attempted* to be activated (in priority/time order).
func (pl *MyCrossNodePreemption) activateBlockedPods(max int) []types.UID {
	_ = pl.pruneStaleSetEntries(pl.Blocked)
	if pl.Blocked == nil || pl.Blocked.Size() == 0 {
		return nil
	}

	// Snapshot and resolve current Pod objects
	snap := pl.Blocked.Snapshot()
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	type item struct {
		p   *v1.Pod
		key PodKey
	}
	items := make([]item, 0, len(snap))
	for _, k := range snap {
		if p, err := l.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, item{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return nil
	}

	// Sort by priority, then creation timestamp (older first), then name
	sort.Slice(items, func(i, j int) bool {
		pi := getPodPriority(items[i].p)
		pj := getPodPriority(items[j].p)
		if pi != pj {
			return pi > pj
		}
		ti := items[i].p.GetCreationTimestamp().Time
		tj := items[j].p.GetCreationTimestamp().Time
		if ti.IsZero() || tj.IsZero() {
			return items[i].p.GetName() < items[j].p.GetName()
		}
		return ti.Before(tj)
	})

	// Limit the number of pods to activate
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}
	if limit == 0 {
		return nil
	}

	// Build activation map and record "tried" UIDs
	toAct := make(map[string]*v1.Pod, limit)
	tried := make([]types.UID, 0, limit)
	for _, it := range items[:limit] {
		toAct[it.p.Namespace+"/"+it.p.Name] = it.p
		tried = append(tried, it.key.UID)
	}

	// Activate and remove only those attempted
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(V2).InfoS("Activated blocked pods", "count", len(toAct), "max", max)
		for _, it := range items[:limit] {
			pl.Blocked.RemovePod(it.key.UID)
		}
	}

	return tried
}

// Activate up to 'max' pods from the batched set; remove only those that were activated
// or explicitly provided via podsToRemove.
func (pl *MyCrossNodePreemption) activateBatchedPods(podsToRemove []*v1.Pod, max int) {
	_ = pl.pruneStaleSetEntries(pl.Batched)
	if pl.Batched == nil || pl.Batched.Size() == 0 {
		return
	}
	snap := pl.Batched.Snapshot()
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	type item struct {
		p   *v1.Pod
		key PodKey
	}
	items := make([]item, 0, len(snap))
	for _, k := range snap {
		if p, err := l.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, item{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return
	}
	// Sort by priority and creation timestamp
	sort.Slice(items, func(i, j int) bool {
		pi := getPodPriority(items[i].p)
		pj := getPodPriority(items[j].p)
		if pi != pj {
			return pi > pj
		}
		ti := items[i].p.GetCreationTimestamp().Time
		tj := items[j].p.GetCreationTimestamp().Time
		if ti.IsZero() || tj.IsZero() {
			return items[i].p.GetName() < items[j].p.GetName()
		}
		return ti.Before(tj)
	})
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}
	toAct := make(map[string]*v1.Pod, limit)
	for _, it := range items[:limit] {
		toAct[it.p.Namespace+"/"+it.p.Name] = it.p
	}
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(V2).InfoS("Activated batched pods", "count", len(toAct), "max", max)
		// Remove only the ones we just activated
		for _, it := range items[:limit] {
			pl.Batched.RemovePod(it.key.UID)
		}
	}
	// If caller passes podsToRemove; remove them from the batched set
	for _, p := range podsToRemove {
		if p != nil {
			pl.Batched.RemovePod(p.UID)
		}
	}
}

// pruneStaleSetEntries removes stale entries from the given pod set.
func (pl *MyCrossNodePreemption) pruneStaleSetEntries(set *PodSet) int {
	removed := pl.pruneSetStale(set, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep function: keep only pending pods
	})
	if removed > 0 {
		klog.V(V2).InfoS("Pruned stale entries", "removed", removed)
	}
	return removed
}

// ---------- Workload Helpers  --------------

// WorkloadKind represents the type of workload.
func (wk WorkloadKey) String() string {
	switch wk.Kind {
	case wkReplicaSet:
		return "rs:" + wk.Namespace + "/" + wk.Name
	case wkStatefulSet:
		return "ss:" + wk.Namespace + "/" + wk.Name
	case wkDaemonSet:
		return "ds:" + wk.Namespace + "/" + wk.Name
	case wkJob:
		return "job:" + wk.Namespace + "/" + wk.Name
	default:
		return wk.Namespace + "/" + wk.Name
	}
}

// topWorkload returns the top-level workload controller of a pod, if any.
func topWorkload(p *v1.Pod) (WorkloadKey, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		switch o.Kind {
		case "ReplicaSet":
			return WorkloadKey{Kind: wkReplicaSet, Namespace: p.Namespace, Name: o.Name}, true
		case "StatefulSet":
			return WorkloadKey{Kind: wkStatefulSet, Namespace: p.Namespace, Name: o.Name}, true
		case "DaemonSet":
			return WorkloadKey{Kind: wkDaemonSet, Namespace: p.Namespace, Name: o.Name}, true
		case "Job":
			return WorkloadKey{Kind: wkJob, Namespace: p.Namespace, Name: o.Name}, true
		}
	}
	return WorkloadKey{}, false
}

// -------------- Quantity Helpers --------------

// bytesToMiB converts bytes to MiB.
func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
}

// computeSolverScore computes final Score from the snapshot given to the solver
// (SolverInput) and the solver plan (SolverOutput).
// It reconstructs the "after" world by applying {placements, evictions} on top of
// the original locations (Where) from the input, then counts:
//   - placed_by_priority: all pods that end up placed (not evicted)
//   - evicted:            len(out.Evictions)
//   - moved:              running→running on a different node (not evicted)
func computeSolverScore(in SolverInput, out *SolverOutput) Score {
	if out == nil {
		return Score{}
	}
	// before-state (where) and priority by UID
	origWhere := make(map[string]string, len(in.Pods)+1)
	pri := make(map[string]int32, len(in.Pods)+1)
	for _, sp := range in.Pods {
		origWhere[sp.UID] = sp.Node
		pri[sp.UID] = sp.Priority
	}
	if in.Preemptor != nil {
		if _, ok := origWhere[in.Preemptor.UID]; !ok {
			origWhere[in.Preemptor.UID] = "" // pending
			pri[in.Preemptor.UID] = in.Preemptor.Priority
		}
	}

	// after-state starts from orig, then apply placements for known UIDs
	afterWhere := make(map[string]string, len(origWhere))
	for uid, w := range origWhere {
		afterWhere[uid] = w
	}
	for _, plm := range out.Placements {
		if plm.ToNode == "" {
			continue
		}
		if _, known := afterWhere[plm.Pod.UID]; known {
			afterWhere[plm.Pod.UID] = plm.ToNode
		}
	}

	// evicted set
	evicted := make(map[string]struct{}, len(out.Evictions))
	for _, e := range out.Evictions {
		evicted[e.Pod.UID] = struct{}{}
	}

	// placed_by_priority
	placedByPri := map[string]int{}
	for uid, after := range afterWhere {
		if _, gone := evicted[uid]; gone {
			continue
		}
		if after != "" {
			key := strconv.Itoa(int(pri[uid]))
			placedByPri[key] = placedByPri[key] + 1
		}
	}

	// moves: running→running and node changed, not evicted
	moves := 0
	for uid, before := range origWhere {
		if _, gone := evicted[uid]; gone {
			continue
		}
		after := afterWhere[uid]
		if before != "" && after != "" && before != after {
			moves++
		}
	}
	return Score{
		PlacedByPriority: placedByPri,
		Evicted:          len(evicted),
		Moved:            moves,
	}
}

// ---------- Plan Helpers ------------

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
	if !pl.IsActivePlan() {
		return false
	}
	ap := pl.getActivePlan()

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

func (pl *MyCrossNodePreemption) IsActivePlan() bool {
	ap := pl.getActivePlan()
	return ap != nil && ap.PlanDoc.Status == PlanStatusActive
}

// onPlanSettled is called when a plan is settled (i.e., all its actions are completed).
func (pl *MyCrossNodePreemption) onPlanSettled(status PlanStatus) bool {
	// Just double-check plan is not already completed
	if !pl.IsActivePlan() {
		return false
	}
	ap := pl.getActivePlan()
	pl.clearActivePlan()
	klog.InfoS("plan settled; deactivating active plan", "planID", ap.ID)
	pl.leaveActive()
	// We do not activate blocked pods when we are in Every@PreEnqueue
	// as it would lead to high contention; instead we periodically nudge them.
	if !optimizeForEvery() || !optimizeAtPreEnqueue() {
		pl.activateBlockedPods(0)
	}
	if ap.Cancel != nil {
		ap.Cancel() // stop the timeout watcher
	}
	_ = pl.markPlanStatus(context.Background(), ap.ID, status)
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

func podsByUID(live []*v1.Pod) map[string]*v1.Pod {
	m := make(map[string]*v1.Pod, len(live))
	for _, p := range live {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		m[string(p.UID)] = p
	}
	return m
}

func (pl *MyCrossNodePreemption) buildActionsFromSolver(
	out *SolverOutput,
	preemptor *v1.Pod, // may be nil
	pods []*v1.Pod, // <-- pre-fetched once per flow
) (evicts []Placement, moves []NewPlacement, oldPlacements []Placement, newPlacements []NewPlacement, nominatedNode string, err error) {
	if out == nil {
		return nil, nil, nil, nil, "", nil
	}

	// Build UID -> *v1.Pod from provided snapshot
	podsByUID := podsByUID(pods)
	if preemptor != nil {
		podsByUID[string(preemptor.UID)] = preemptor
	}

	// ---------------- Old placements (ALL running pods in the snapshot) ----------------
	oldPlacements = make([]Placement, 0, len(podsByUID))
	for _, p := range podsByUID {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if node := p.Spec.NodeName; node != "" {
			oldPlacements = append(oldPlacements, Placement{
				Pod:  Pod{UID: string(p.UID), Namespace: p.Namespace, Name: p.Name},
				Node: node,
			})
		}
	}
	sort.Slice(oldPlacements, func(i, j int) bool {
		if oldPlacements[i].Pod.Namespace != oldPlacements[j].Pod.Namespace {
			return oldPlacements[i].Pod.Namespace < oldPlacements[j].Pod.Namespace
		}
		return oldPlacements[i].Pod.Name < oldPlacements[j].Pod.Name
	})

	// ---------------- Evictions ----------------
	for _, e := range out.Evictions {
		if p := podsByUID[e.Pod.UID]; p != nil {
			evicts = append(evicts, Placement{
				Pod:  Pod{UID: string(p.UID), Namespace: p.Namespace, Name: p.Name},
				Node: p.Spec.NodeName,
			})
		}
	}

	// ---------------- Moves + new placements + preemptor's nominated node ----------------
	for _, plm := range out.Placements {
		p := podsByUID[plm.Pod.UID]
		if p == nil || plm.ToNode == "" {
			continue
		}

		src := p.Spec.NodeName
		isPreemptor := preemptor != nil && string(preemptor.UID) == plm.Pod.UID
		_, owned := topWorkload(p) // true if controller-owned (RS/SS/DS/Job)

		// newPlacements: only preemptor or standalone
		if isPreemptor || !owned {
			if src == "" || src != plm.ToNode {
				newPlacements = append(newPlacements, NewPlacement{
					Pod:      Pod{UID: string(p.UID), Namespace: p.Namespace, Name: p.Name},
					FromNode: src,
					ToNode:   plm.ToNode,
				})
				if isPreemptor {
					nominatedNode = plm.ToNode
				}
			}
		}

		// moves: any pod that actually changes nodes (exclude preemptor)
		if src != "" && src != plm.ToNode && !isPreemptor {
			moves = append(moves, NewPlacement{
				Pod:      Pod{UID: string(p.UID), Namespace: p.Namespace, Name: p.Name},
				FromNode: src,
				ToNode:   plm.ToNode,
			})
		}
	}

	return evicts, moves, oldPlacements, newPlacements, nominatedNode, nil
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
// By checking this for every pod we process in PostBind, we can be 100% sure
// we have the correct state in cache before completing the plan.
func (pl *MyCrossNodePreemption) waitPendingBoundInCache(
	ctx context.Context,
	pending *v1.Pod,
) (bool, error) {
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	ns := pending.Namespace
	name := pending.Name
	// Single-shot check
	p, err := l.Pods(ns).Get(name)
	if err == nil && p.Spec.NodeName != "" {
		return true, nil
	}
	// Poll until the cached pod is bound.
	err = wait.PollUntilContextTimeout(ctx, PlanPendingBindInterval, PlanExecutionTimeout, true, func(ctx context.Context) (bool, error) {
		p, err := l.Pods(ns).Get(name)
		if apierrors.IsNotFound(err) { // pod not found; keep polling
			klog.V(V2).InfoS("Waiting for pending to appear in cache", "pod", ns+"/"+name)
			return false, nil
		}
		if err != nil { // Lister error; keep polling
			klog.V(V2).InfoS("Lister error while waiting for pending", "pod", ns+"/"+name, "err", err)
			return false, nil
		}
		if p.UID != pending.UID { // waiting for matching pending UID
			klog.V(V2).InfoS("Waiting for matching pending UID", "pod", ns+"/"+name, "wantUID", pending.UID, "haveUID", p.UID)
			return false, nil
		}
		if p.Spec.NodeName == "" { // waiting for preemptor to bind
			klog.V(V2).InfoS("Waiting for pending to bind", "pod", ns+"/"+name)
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
// Mode: In-batches: All pods bound to target nodes (only B, C).
// helpers.go
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, ap *ActivePlanState, pod *v1.Pod) (bool, error) {
	if ap == nil || ap.PlanDoc == nil {
		// Plan got torn down concurrently; treat as "not completed yet" (retry later)
		klog.V(V2).InfoS("Plan completion check skipped: no active plan doc")
		return false, nil
	}

	// Wait until the pod is visible+bound in cache when relevant.
	ok, err := pl.waitPendingBoundInCache(ctx, pod)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()

	// A) Standalone/preemptor pods planned to specific nodes must be there.
	for _, plm := range ap.PlanDoc.PlacementByName {
		po, err := podLister.Pods(plm.Pod.Namespace).Get(plm.Pod.Name)
		if err != nil {
			klog.InfoS("Plan incomplete: standalone/preemptor pod missing",
				"pod", plm.Pod.Namespace+"/"+plm.Pod.Name, "expectedNode", plm.ToNode) // TODO_HC:DEBUG
			if apierrors.IsNotFound(err) {
				klog.V(V2).InfoS("Plan incomplete: standalone/preemptor pod missing",
					"pod", plm.Pod.Namespace+"/"+plm.Pod.Name, "expectedNode", plm.ToNode)
				return false, nil
			}
			return false, fmt.Errorf("get pod %s/%s from lister: %w", plm.Pod.Namespace, plm.Pod.Name, err)
		}
		// Only enforce for standalone and preemptor; workload pods handled next.
		_, owned := topWorkload(po)
		if owned && (ap.PlanDoc.Preemptor == nil || string(po.UID) != ap.PlanDoc.Preemptor.Pod.UID) {
			continue
		}
		if po.DeletionTimestamp != nil || po.Spec.NodeName != plm.ToNode {
			klog.V(V2).InfoS("Plan incomplete: standalone/preemptor pod mismatch",
				"pod", combineNsName(plm.Pod.Namespace, plm.Pod.Name),
				"expectedNode", plm.ToNode, "haveNode", po.Spec.NodeName)
			return false, nil
		}
	}

	// B) Per-workload per-node quotas consumed.
	for wk, perNode := range ap.WorkloadPerNodeCnts {
		for node, ctr := range perNode {
			if ctr.Load() > 0 {
				klog.V(V2).InfoS("Plan incomplete: remaining quota",
					"workload", wk, "node", node, "remaining", ctr.Load())
				return false, nil
			}
		}
	}

	return true, nil
}

// getActivePlan returns the currently active plan, if any.
func (pl *MyCrossNodePreemption) getActivePlan() *ActivePlanState {
	return pl.ActivePlan.Load()
}

// clearActivePlan clears the currently active plan, if any.
func (pl *MyCrossNodePreemption) clearActivePlan() {
	pl.ActivePlan.Store(nil)
}

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(_ context.Context) ([]v1.ConfigMap, error) {
	sel := labels.SelectorFromSet(labels.Set{PlanConfigMapLabelKey: "true"})
	l := pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(PlanConfigMapNamespace)
	items, err := l.List(sel)
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

// markPlanStatus sets the Status (Active/Completed/Failed) in plan.json.
// It will clear it for Active.
// If the plan is already in a final state (Completed/Failed), it won't be downgraded.
func (pl *MyCrossNodePreemption) markPlanStatus(ctx context.Context, cmName string, status PlanStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		l := pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().
			Lister().ConfigMaps(PlanConfigMapNamespace)
		cm, err := l.Get(cmName)
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
		// If already final, don't overwrite with a different status.
		if sp.Status == PlanStatusCompleted || sp.Status == PlanStatusFailed {
			return nil
		}
		sp.Status = status
		b, _ := json.MarshalIndent(&sp, "", "  ")
		patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
		_, err = pl.Client.CoreV1().ConfigMaps(PlanConfigMapNamespace).
			Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
}

type WorkloadQuotas map[string]map[string]int32

// computeWorkloadQuotasFromPlan builds a JSON-serializable quotas doc
// from the plan's Moves and the live pods snapshot.
func computeWorkloadQuotasFromPlan(sp *StoredPlan, pods []*v1.Pod) WorkloadQuotas {
	if sp == nil {
		return nil
	}
	podMap := podsByUID(pods)
	quotas := WorkloadQuotas{}

	for _, mv := range sp.Moves {
		p := podMap[mv.Pod.UID]
		if p == nil || string(p.UID) != mv.Pod.UID {
			continue
		}
		if wk, owned := topWorkload(p); owned {
			wkKey := wk.String()
			if quotas[wkKey] == nil {
				quotas[wkKey] = map[string]int32{}
			}
			quotas[wkKey][mv.ToNode] = quotas[wkKey][mv.ToNode] + 1
		}
		// standalone "moves" are pinned by name (not part of quotas)
	}
	return quotas
}

// watchPlanTimeout monitors the timeout for the given active plan.
func (pl *MyCrossNodePreemption) watchPlanTimeout(ap *ActivePlanState) {
	<-ap.Ctx.Done()
	// If Cancel() was called due to completion/replacement, do nothing.
	if ap.Ctx.Err() != context.DeadlineExceeded {
		return
	}
	// Ensure we're still looking at the same active plan
	cur := pl.getActivePlan()
	if cur == nil || cur.ID != ap.ID || cur.PlanDoc.Status != PlanStatusActive {
		return
	}
	klog.InfoS("plan timeout reached; deactivating plan", "planID", ap.ID, "ttl", PlanExecutionTimeout)
	pl.onPlanSettled(PlanStatusFailed)
}

// setActivePlan sets the given stored plan as the active plan and initializes its counters,
// deriving both WorkloadPerNodeCnts and PlacementByName solely from NewPlacements.
// For controller-owned pods, quotas are keyed by the controller (e.g., ReplicaSet) name.
func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string, pods []*v1.Pod) {
	if sp == nil {
		return
	}

	remaining := make(WorkloadPerNodeCnts) // workload -> node -> *atomic.Int32
	byName := make(map[string]string, len(sp.PlacementByName)+1)
	podMap := podsByUID(pods)

	// Pin preemptor/standalone by exact name (from NewPlacements)
	for _, plm := range sp.PlacementByName {
		if p := podMap[plm.Pod.UID]; p != nil && string(p.UID) == plm.Pod.UID {
			byName[combineNsName(plm.Pod.Namespace, plm.Pod.Name)] = plm.ToNode
		}
	}
	if sp.Preemptor != nil && sp.Preemptor.NominatedNode != "" {
		byName[combineNsName(sp.Preemptor.Pod.Namespace, sp.Preemptor.Pod.Name)] = sp.Preemptor.NominatedNode
	}

	if sp.WorkloadQuotasDoc != nil {
		for wk, perNode := range sp.WorkloadQuotasDoc {
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
	}

	if old := pl.getActivePlan(); old != nil && old.Cancel != nil {
		old.Cancel()
	}
	ctxPlan, cancel := context.WithTimeout(context.Background(), PlanExecutionTimeout)
	ap := &ActivePlanState{
		ID:                  id,
		PlanDoc:             sp,
		WorkloadPerNodeCnts: remaining,
		PlacementByName:     byName,
		Ctx:                 ctxPlan,
		Cancel:              cancel,
	}
	pl.ActivePlan.Store(ap)
	go pl.watchPlanTimeout(ap)
}

// ------------- Solver Helpers --------------

// check that at least one solver is enabled
func (pl *MyCrossNodePreemption) isSolverEnabled() bool {
	return SolverPythonEnabled || SolverBfsEnabled || SolverLocalSearchEnabled
}

// fillNodesAndPods adds nodes/pods using SharedInformerFactory listers.
// If batched != nil, pending batched pods are appended with where="" (and preemptor can be nil).
func (pl *MyCrossNodePreemption) fillNodesAndPods(
	in *SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	includePending bool,
) error {
	// Nodes
	usable := map[string]bool{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name:        n.Name,
			CapCPUm:     n.Status.Allocatable.Cpu().MilliValue(),
			CapMemBytes: n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}
	// Pods
	preUID := ""
	if preemptor != nil {
		preUID = string(preemptor.UID)
	}
	seen := make(map[string]bool, len(pods)+len(batched))
	for _, p := range pods {
		// Skip the preemptor (it is provided via in.Preemptor)
		if string(p.UID) == preUID {
			continue
		}
		where := p.Spec.NodeName
		if where == "" {
			// Only include pending pods when explicitly asked (cohort mode).
			if !includePending {
				continue
			}
		} else {
			// If the pod is already bound to a node, ensure that node is usable.
			if !usable[where] {
				continue
			}
		}
		sp := toPodType(p, where)
		if p.Namespace == "kube-system" {
			sp.Protected = true
		}
		if !seen[sp.UID] {
			in.Pods = append(in.Pods, sp)
			seen[sp.UID] = true
		}
	}
	// Cohort: append the batched pending pods (where = ""), if requested
	if includePending {
		for _, p := range batched {
			if p == nil {
				continue
			}
			if string(p.UID) == preUID {
				continue
			}
			sp := toPodType(p, "")
			if p.Namespace == "kube-system" {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}
	}
	return nil
}

// buildSolverInput builds the common input for either batch or single.
func (pl *MyCrossNodePreemption) buildSolverInput(mode SolveMode, nodes []*v1.Node, pods []*v1.Pod, preemptor *v1.Pod, batched []*v1.Pod) (SolverInput, error) {
	in := SolverInput{
		IgnoreAffinity: true,
		LogProgress:    SolverLogProgress,
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
		Mode:           SolverMode,
		TimeoutMs:      0, // will be filled outside
	}
	switch mode {
	case SolveSingle:
		if preemptor == nil {
			return SolverInput{}, fmt.Errorf("SolveSingle requires preemptor")
		}
		pre := toPodType(preemptor, "")
		in.Preemptor = &pre
		if err := pl.fillNodesAndPods(&in, nodes, pods, preemptor, nil, false); err != nil {
			return SolverInput{}, fmt.Errorf("fill (single): %w", err)
		}
	case SolveBatch:
		if err := pl.fillNodesAndPods(&in, nodes, pods, nil, batched, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (batch): %w", err)
		}
	case SolveContinuously:
		if err := pl.fillNodesAndPods(&in, nodes, pods, nil, nil, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (continuous): %w", err)
		}
	default:
		return SolverInput{}, fmt.Errorf("unknown solve mode")
	}
	if len(in.Nodes) == 0 {
		return SolverInput{}, fmt.Errorf("no usable Ready nodes available; waiting")
	}
	return in, nil
}

// IsSolverFeasible checks if the solver output is feasible.
// OPTIMAL means the solution is perfect and meets all constraints (note there can be multiple optimal solutions and that the solver is non-deterministic).
// FEASIBLE means the solution is not perfect but still meets all constraints.
func IsSolverFeasible(out *SolverOutput) bool {
	return out != nil && (out.Status == "OPTIMAL" || out.Status == "FEASIBLE")
}

// toPodType converts a Pod to a PodType.
func toPodType(p *v1.Pod, node string) SolverPod {
	return SolverPod{
		UID:         string(p.UID),
		Namespace:   p.Namespace,
		Name:        p.Name,
		ReqCPUm:     getPodCPURequest(p),
		ReqMemBytes: getPodMemoryRequest(p),
		Priority:    getPodPriority(p),
		Node:        node,
	}
}

// comparePlaced returns 1 if a>b, -1 if a<b, 0 if equal (lexi by priority desc).
func comparePlaced(a, b map[string]int) int {
	keys := map[int]struct{}{}
	for k := range a {
		if v, err := strconv.Atoi(k); err == nil {
			keys[v] = struct{}{}
		}
	}
	for k := range b {
		if v, err := strconv.Atoi(k); err == nil {
			keys[v] = struct{}{}
		}
	}
	prs := make([]int, 0, len(keys))
	for k := range keys {
		prs = append(prs, k)
	}
	// sort priorities descending
	sort.Sort(sort.Reverse(sort.IntSlice(prs)))
	for _, pr := range prs {
		ai := a[strconv.Itoa(pr)]
		bi := b[strconv.Itoa(pr)]
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

// cmpInt returns +1 if a<b (improvement because smaller is better),
// -1 if a>b (worse), 0 if equal.
func cmpInt(suggested, baseline int) int {
	switch {
	case suggested < baseline:
		return 1
	case suggested > baseline:
		return -1
	default:
		return 0
	}
}

// IsImprovement compares two scores lexicographically:
// 1) More placed per priority (lexicographic map compare)
// 2) Fewer evictions
// 3) Fewer moves
// Returns 1 if suggested is better, -1 if worse, 0 if equal.
func IsImprovement(baseline, suggested Score) int {
	// 1) Placed-by-priority (more is better)
	if cmp := comparePlaced(suggested.PlacedByPriority, baseline.PlacedByPriority); cmp != 0 {
		klog.V(V2).InfoS("Compare placed-by-priority", "result", cmp,
			"suggested", suggested.PlacedByPriority, "baseline", baseline.PlacedByPriority)
		return cmp
	}
	// 2) Evictions (fewer is better)
	if cmp := cmpInt(suggested.Evicted, baseline.Evicted); cmp != 0 {
		klog.V(V2).InfoS("Compare evictions", "result", cmp,
			"suggested", suggested.Evicted, "baseline", baseline.Evicted)
		return cmp
	}
	// 3) Moves (fewer is better)
	if cmp := cmpInt(suggested.Moved, baseline.Moved); cmp != 0 {
		klog.V(V2).InfoS("Compare moves", "result", cmp,
			"suggested", suggested.Moved, "baseline", baseline.Moved)
		return cmp
	}
	// Equal on all metrics
	klog.V(V2).InfoS("No change: equal on placed, evictions, and moves")
	return 0
}

// buildInputAndBaseline builds the exact snapshot we send to the solver,
// and returns the baseline and a digest for concurrency checks.
func (pl *MyCrossNodePreemption) buildInputAndBaseline(
	mode SolveMode,
	nodes []*v1.Node, // <-- pre-fetched once per flow
	pods []*v1.Pod, // <-- pre-fetched once per flow
	preemptor *v1.Pod,
	batched []*v1.Pod,
) (SolverInput, Score, string, error) {
	in, err := pl.buildSolverInput(mode, nodes, pods, preemptor, batched)
	if err != nil {
		return SolverInput{}, Score{}, "", err
	}
	baseline := computeBaselineFromInput(in)
	digest := buildDigestFromInput(in)
	return in, baseline, digest, nil
}

// computeBaselineFromInput computes the baseline score from the solver input.
func computeBaselineFromInput(in SolverInput) Score {
	placedByPri := map[string]int{}
	for _, sp := range in.Pods {
		if sp.Node == "" {
			continue // pending doesn't count into "placed"
		}
		pr := strconv.Itoa(int(sp.Priority))
		placedByPri[pr] = placedByPri[pr] + 1
	}
	return Score{
		PlacedByPriority: placedByPri,
		Evicted:          0,
		Moved:            0,
	}
}

// buildDigestFromInput produces a deterministic hash of the snapshot that fed the solver input.
// We use the already-normalized SolverInput (nodes/pods) for stability.
func buildDigestFromInput(in SolverInput) string {
	h := sha256.New()
	// nodes sorted by name
	ns := make([]SolverNode, len(in.Nodes))
	copy(ns, in.Nodes)
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	for _, n := range ns {
		h.Write([]byte(n.Name))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(n.CapCPUm, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(n.CapMemBytes, 10)))
		h.Write([]byte("\n"))
	}
	// pods sorted by UID
	ps := make([]SolverPod, len(in.Pods))
	copy(ps, in.Pods)
	sort.Slice(ps, func(i, j int) bool { return ps[i].UID < ps[j].UID })
	for _, p := range ps {
		h.Write([]byte(p.UID))
		h.Write([]byte("|"))
		h.Write([]byte(p.Namespace))
		h.Write([]byte("|"))
		h.Write([]byte(p.Name))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(p.ReqCPUm, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(p.ReqMemBytes, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(int64(p.Priority), 10)))
		h.Write([]byte("|"))
		h.Write([]byte(p.Node))
		h.Write([]byte("|"))
		if p.Protected {
			h.Write([]byte("1"))
		} else {
			h.Write([]byte("0"))
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
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

// --------- Environment Variables Helpers ----------

// getenv retrieves the value of an environment variable or returns a default value.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseBool parses a boolean string and returns the corresponding bool value.
func parseBool(s string) bool {
	v, _ := strconv.ParseBool(s)
	return v
}

// parseInt parses an integer string and returns the corresponding int value.
func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// parseTime parses a duration string and returns the corresponding time.Duration.
func parseTime(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 0
}

// parseCadence parses a cadence string and returns the corresponding OptimizationCadenceMode.
func parseCadence(s string) OptimizationCadenceMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "for_every":
		return OptimizeForEvery
	case "in_batches":
		return OptimizeInBatches
	case "continuously":
		return OptimizeContinuously
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_CADENCE value; defaulting to in_batches", "value", s)
		return OptimizeContinuously
	}
}

// parseOptimizeAt parses an optimization "at" string and returns the corresponding OptimizationAtMode.
func parseOptimizeAt(s string) OptimizationAtMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "preenqueue":
		return OptimizeAtPreEnqueue
	case "postfilter":
		return OptimizeAtPostFilter
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_AT value; defaulting to postfilter", "value", s)
		return OptimizeAtPostFilter
	}
}

// preparedState caches one build of cluster state + worklist.
// Each solver run should mutate a fresh clone, not this base.
type preparedState struct {
	nodes    map[string]*SolverNode
	pods     map[string]*SolverPod
	order    []*SolverNode
	pre      *SolverPod
	worklist []*SolverPod
	single   bool
	moveGate *int32
}

func prepareState(in SolverInput) *preparedState {
	nodes, pods, pending, order, pre := buildClusterState(in)
	wl, single, mg := buildWorklist(pending, pre)
	return &preparedState{
		nodes:    nodes,
		pods:     pods,
		order:    order,
		pre:      pre,
		worklist: wl,
		single:   single,
		moveGate: mg,
	}
}

// freshClone returns deep-ish cloned nodes/pods/order and re-materializes
// the worklist against the cloned pods, so solvers can mutate safely.
func (ps *preparedState) freshClone() (
	nodes map[string]*SolverNode,
	pods map[string]*SolverPod,
	order []*SolverNode,
	worklist []*SolverPod,
) {
	// 1) clone pods
	pods = make(map[string]*SolverPod, len(ps.pods))
	for uid, p0 := range ps.pods {
		cp := *p0
		pods[uid] = &cp
	}
	// 2) clone nodes (+ wire cloned pods into cloned nodes)
	nodes = make(map[string]*SolverNode, len(ps.nodes))
	order = make([]*SolverNode, 0, len(ps.order))
	for _, n0 := range ps.order {
		n := &SolverNode{
			Name:          n0.Name,
			CapCPUm:       n0.CapCPUm,
			CapMemBytes:   n0.CapMemBytes,
			Labels:        n0.Labels,
			AllocCPUm:     n0.AllocCPUm,
			AllocMemBytes: n0.AllocMemBytes,
			Pods:          make(map[string]*SolverPod, len(n0.Pods)),
		}
		for uid := range n0.Pods {
			if p := pods[uid]; p != nil {
				n.Pods[uid] = p
				p.Node = n.Name // reflect current location
			}
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	// 3) project worklist to cloned pods by UID
	worklist = make([]*SolverPod, len(ps.worklist))
	for i, p0 := range ps.worklist {
		worklist[i] = pods[p0.UID]
	}
	return
}
