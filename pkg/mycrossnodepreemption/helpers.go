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
	if pl.Active.Load() {
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

func (pl *MyCrossNodePreemption) WaitForInformersSynced(ctx context.Context, podsInf, nodesInf, cmsInf, rsInf, ssInf, dsInf, jobInf cache.SharedIndexInformer) {
	if cache.WaitForCacheSync(ctx.Done(),
		podsInf.HasSynced, nodesInf.HasSynced, cmsInf.HasSynced,
		rsInf.HasSynced, ssInf.HasSynced, dsInf.HasSynced, jobInf.HasSynced,
	) {
		if CacheWarmupAfterDelay > 0 {
			klog.InfoS("Cache warm-up delay", "duration", CacheWarmupAfterDelay)
			select {
			case <-time.After(CacheWarmupAfterDelay):
			case <-ctx.Done():
				// Activate the pods we have blocked while waiting
				pl.activateBlockedPods(0)
				return
			}
		}
		pl.CachesWarm.Store(true)
		klog.InfoS("Caches marked ready")
	} else {
		klog.InfoS("Cache sync aborted (context done)")
	}
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

// splitNamespaceName splits a namespace/name string into its constituent parts.
func splitNamespaceName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
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
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.UID = ""
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
func (pl *MyCrossNodePreemption) activateBlockedPods(max int) {
	_ = pl.pruneStaleSetEntries(pl.Blocked)
	if pl.Blocked == nil || pl.Blocked.Size() == 0 {
		return
	}
	// We need to know which UIDs we activated to remove only those.
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
	// Limit the number of pods to activate
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}
	// Map of pods to activate
	toAct := make(map[string]*v1.Pod, limit)
	for _, it := range items[:limit] {
		toAct[it.p.Namespace+"/"+it.p.Name] = it.p
	}
	// Activate the selected pods
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(V2).InfoS("Activated blocked pods", "count", len(toAct), "max", max)
		// Remove only the activated ones from the set
		for _, it := range items[:limit] {
			pl.Blocked.RemovePod(it.key.UID)
		}
	}
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

// workloadEqual returns true if two workload keys are equal.
func workloadEqual(a, b WorkloadKey) bool {
	return a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

// workloadSelector returns the label selector for a workload.
func (pl *MyCrossNodePreemption) workloadSelector(wk WorkloadKey) (metav1.LabelSelector, error) {
	switch wk.Kind {
	case wkReplicaSet:
		rs, err := pl.Handle.SharedInformerFactory().
			Apps().V1().ReplicaSets().Lister().
			ReplicaSets(wk.Namespace).Get(wk.Name)
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *rs.Spec.Selector, nil
	case wkStatefulSet:
		ss, err := pl.Handle.SharedInformerFactory().
			Apps().V1().StatefulSets().Lister().
			StatefulSets(wk.Namespace).Get(wk.Name)
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ss.Spec.Selector, nil
	case wkDaemonSet:
		ds, err := pl.Handle.SharedInformerFactory().
			Apps().V1().DaemonSets().Lister().
			DaemonSets(wk.Namespace).Get(wk.Name)
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ds.Spec.Selector, nil
	case wkJob:
		job, err := pl.Handle.SharedInformerFactory().
			Batch().V1().Jobs().Lister().
			Jobs(wk.Namespace).Get(wk.Name)
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		if job.Spec.Selector != nil {
			return *job.Spec.Selector, nil
		}
		return metav1.LabelSelector{MatchLabels: job.Spec.Template.Labels}, nil
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind: %v", wk.Kind)
	}
}

// workloadParseKey parses a workload key from its string representation.
func workloadParseKey(s string) (WorkloadKey, bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return WorkloadKey{}, false
	}
	kindStr, rest := s[:colon], s[colon+1:]
	ns, name := splitNamespaceName(rest)
	var k WorkloadKind
	switch kindStr {
	case "rs":
		k = wkReplicaSet
	case "ss":
		k = wkStatefulSet
	case "ds":
		k = wkDaemonSet
	case "job":
		k = wkJob
	default:
		return WorkloadKey{}, false
	}
	return WorkloadKey{Kind: k, Namespace: ns, Name: name}, true
}

// -------------- Quantity Helpers --------------

// bytesToMiB converts bytes to MiB.
func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
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
func (pl *MyCrossNodePreemption) allowedByActivePlan(pod *v1.Pod) bool {
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc.Completed {
		return false
	}
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		return true
	}
	if _, ok := ap.PlanDoc.PlacementsByName[pod.Namespace+"/"+pod.Name]; ok {
		return true
	}
	if wk, ok := topWorkload(pod); ok {
		if _, in := ap.PlanDoc.WkDesiredPerNode[wk.String()]; in {
			return true
		}
	}
	return false
}

// onPlanSettled is called when a plan is settled (i.e., all its actions are completed).
func (pl *MyCrossNodePreemption) onPlanSettled() bool {
	// Just double-check plan is not already completed
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc.Completed {
		return false
	}
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
	pl.markPlanCompleted(context.Background(), ap.ID)
	return true
}

// exportPlanToConfigMap exports the given plan to a ConfigMap.
func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	name string,
	plan *Plan,
	out *SolverOutput,
	pending *v1.Pod, // may be nil
	placementsByName map[string]string,
	workloadDesired map[string]map[string]int,
) error {
	doc := &StoredPlan{
		Completed:        false,
		CompletedAt:      nil,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		Mode:             modeToString(),
		SolverOutput:     out,
		Plan:             *plan,
		PlacementsByName: placementsByName,
		WkDesiredPerNode: workloadDesired,
	}
	// Fill single-preemptor metadata only when provided
	if pending != nil {
		doc.PendingPod = fmt.Sprintf("%s/%s", pending.Namespace, pending.Name)
		doc.PendingUID = string(pending.UID)
		doc.TargetNode = plan.TargetNode // may be empty, kept for symmetry
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// Make a unique name
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
	_ = pl.pruneOldPlans(ctx, PlansToRetain) // prune old plans
	return nil
}

// translatePlanFromSolver translates the solver output into a plan format.
func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	out *SolverOutput,
	pending *v1.Pod, // may be nil
) (*Plan, error) {
	plan := &Plan{TargetNode: out.NominatedNode}
	// Build quick UID -> *v1.Pod map
	podsByUID := map[string]*v1.Pod{}
	live, err := pl.getPods()
	if err != nil {
		return nil, err
	}
	for _, p := range live {
		podsByUID[string(p.UID)] = p
	}
	if pending != nil {
		podsByUID[string(pending.UID)] = pending
	}
	// Evictions
	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.Evicts = append(plan.Evicts, Evict{
				Pod:      PodRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)},
				FromNode: p.Spec.NodeName,
				CPUm:     getPodCPURequest(p),
				MemBytes: getPodMemoryRequest(p),
			})
		}
	}
	// Moves
	for uid, dest := range out.Placements {
		// Skip preemptor only when we actually have one
		if pending != nil && uid == string(pending.UID) {
			continue
		}
		p, ok := podsByUID[uid]
		if !ok {
			continue
		}
		from := p.Spec.NodeName
		if from == "" || dest == "" || from == dest {
			continue
		}
		plan.Moves = append(plan.Moves, Move{
			Pod:      PodRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)},
			FromNode: from,
			ToNode:   dest,
			CPUm:     getPodCPURequest(p),
			MemBytes: getPodMemoryRequest(p),
		})
	}
	return plan, nil
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
		if json.Unmarshal([]byte(raw), &sp) == nil && !sp.Completed {
			latestIncomplete = items[i].Name
			break
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
			klog.V(V2).InfoS("Waiting for preemptor to appear in cache", "pod", ns+"/"+name)
			return false, nil
		}
		if err != nil { // Lister error; keep polling
			klog.V(V2).InfoS("Lister error while waiting for preemptor", "pod", ns+"/"+name, "err", err)
			return false, nil
		}
		if p.UID != pending.UID { // waiting for matching preemptor UID
			klog.V(V2).InfoS("Waiting for matching preemptor UID", "pod", ns+"/"+name, "wantUID", pending.UID, "haveUID", p.UID)
			return false, nil
		}
		if p.Spec.NodeName == "" { // waiting for preemptor to bind
			klog.V(V2).InfoS("Waiting for preemptor to bind", "pod", ns+"/"+name)
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
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, sp *StoredPlan, pod *v1.Pod) (bool, error) {
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	ok, err := pl.waitPendingBoundInCache(ctx, pod)
	if err != nil {
		return false, err
	}
	if !ok {
		// Still not bound in cache within the window — try again later.
		return false, nil
	}
	// A) Single preemptor pod bound to target node
	if sp.TargetNode != "" && sp.PendingPod != "" {
		pns, pname := splitNamespaceName(sp.PendingPod)
		preemptor, err := podLister.Pods(pns).Get(pname)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get pending pod from lister: %w", err)
		}
		// If it's present but not yet bound to the target, plan is not complete.
		if err == nil && preemptor.Spec.NodeName != sp.TargetNode {
			klog.V(V2).InfoS("Plan incomplete: preemptor not yet bound",
				"pod", sp.PendingPod, "wantNode", sp.TargetNode, "haveNode", preemptor.Spec.NodeName)
			return false, nil
		}
	}
	// B) Standalone/name-addressed pods on specified node
	for fullName, wantNode := range sp.PlacementsByName {
		ns, name := splitNamespaceName(fullName) // <— use the entry's own namespace/name
		pod, err := podLister.Pods(ns).Get(name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(V2).InfoS("Plan incomplete: standalone pod missing",
					"pod", ns+"/"+name, "expectedNode", wantNode)
				return false, nil
			}
			return false, fmt.Errorf("get pod %s/%s from lister: %w", ns, name, err)
		}
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName != wantNode {
			klog.V(V2).InfoS("Plan incomplete: standalone pod mismatch",
				"pod", ns+"/"+name, "expectedNode", wantNode, "haveNode", pod.Spec.NodeName)
			return false, nil
		}
	}
	// C) Per-workload per-node quotas satisfied.
	for wkStr, perNode := range sp.WkDesiredPerNode {
		wk, ok := workloadParseKey(wkStr)
		if !ok {
			continue
		}
		lblSel, err := pl.workloadSelector(wk)
		if err != nil {
			return false, fmt.Errorf("selector for %s: %w", wk.String(), err)
		}
		selector, err := metav1.LabelSelectorAsSelector(&lblSel)
		if err != nil {
			return false, fmt.Errorf("build selector for %s: %w", wk.String(), err)
		}
		// List from the pod lister (namespace-scoped) using the selector
		pods, err := podLister.Pods(wk.Namespace).List(selector)
		if err != nil {
			return false, fmt.Errorf("list pods for %s from lister: %w", wk.String(), err)
		}
		counts := map[string]int{}
		for _, p := range pods {
			if p == nil || p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
				continue
			}
			// Skip the plan's preemptor: it is not part of WkDesiredPerNode by design.
			if sp.TargetNode != "" && string(p.UID) == sp.PendingUID {
				continue
			}
			// Ensure we only count pods that actually belong to this workload
			if twk, ok := topWorkload(p); !ok || !workloadEqual(twk, wk) {
				continue
			}
			counts[p.Spec.NodeName]++
		}
		for node, want := range perNode {
			if have := counts[node]; have != want {
				klog.V(V2).InfoS("Plan incomplete: workload count mismatch",
					"workload", wk.String(), "node", node, "want", want, "have", have,
					"note", "counts exclude preemptor")
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

// markPlanCompleted sets Completed=true in json (i.e. not active plan).
func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
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
			klog.ErrorS(err, "markPlanCompleted: cannot decode plan.json", "configMap", cmName)
			return nil
		}
		if sp.Completed {
			return nil
		}
		now := time.Now().UTC()
		sp.Completed = true
		sp.CompletedAt = &now
		b, _ := json.MarshalIndent(&sp, "", "  ")
		patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
		_, err = pl.Client.CoreV1().ConfigMaps(PlanConfigMapNamespace).
			Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err := pl.pruneOldPlans(context.Background(), PlansToRetain); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
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
	if cur == nil || cur.ID != ap.ID || cur.PlanDoc.Completed {
		return
	}
	klog.InfoS("plan timeout reached; deactivating plan", "planID", ap.ID, "ttl", PlanExecutionTimeout)
	// Mark completed for auditing, then settle
	pl.markPlanCompleted(context.Background(), ap.ID)
	pl.onPlanSettled()
}

// setActivePlan sets the given stored plan as the active plan and initializes its counters.
func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string) {
	// Fast exit: nothing to track
	if sp == nil {
		return
	}

	// --------------- Prepare bookkeeping sets ---------------
	// Track only workloads present in the desired map.
	desired := sp.WkDesiredPerNode // read-only while active
	trackedWK := make(map[string]struct{}, len(desired))
	for wk := range desired {
		trackedWK[wk] = struct{}{}
	}

	// --------------- Single pass over current pods ---------------
	pods, err := pl.getPods()
	if err != nil {
		klog.ErrorS(err, "setActivePlan: getPods failed; installing plan with empty counters")
	}
	// cur[wk][node] = count currently running that belong to tracked workloads
	cur := make(map[string]map[string]int, len(desired))
	// podsByUID is used to compute "plannedOut" from moves/evicts.
	podsByUID := make(map[string]*v1.Pod, len(pods))
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		podsByUID[string(p.UID)] = p
		// Count only running workload pods that are part of tracked workloads
		if p.Spec.NodeName == "" {
			continue
		}
		if wk, ok := topWorkload(p); ok {
			key := wk.String()
			if _, tr := trackedWK[key]; !tr {
				continue
			}
			m, ok := cur[key]
			if !ok {
				m = make(map[string]int, 2)
				cur[key] = m
			}
			m[p.Spec.NodeName]++
		}
	}

	// --------------- Planned-out counts (from solver plan) ---------------
	// plannedOut[wk][node] = how many will be removed from node by this plan
	plannedOut := make(map[string]map[string]int, 8)
	addOut := func(uid string) {
		if p := podsByUID[uid]; p != nil && p.Spec.NodeName != "" {
			if wk, ok := topWorkload(p); ok {
				key := wk.String()
				if _, tr := trackedWK[key]; !tr {
					return
				}
				m, ok := plannedOut[key]
				if !ok {
					m = make(map[string]int, 1)
					plannedOut[key] = m
				}
				m[p.Spec.NodeName]++
			}
		}
	}
	for _, mv := range sp.Plan.Moves {
		addOut(mv.Pod.UID)
	}
	for _, ev := range sp.Plan.Evicts {
		addOut(ev.Pod.UID)
	}

	// --------------- Remaining per workload/node ---------------
	// remaining[wk][node] = max(0, want - have + out)
	// want: how many are desired to be running on the node
	// have: how many are currently running on the node
	// out: how many will be removed from the node by this plan
	remaining := make(WorkloadNodeCounters, len(desired))
	for wk, perNode := range desired {
		nodeMap := make(map[string]*atomic.Int32, len(perNode))
		curWk := cur[wk]
		outWk := plannedOut[wk]
		for node, want := range perNode {
			have := 0
			if curWk != nil {
				have = curWk[node]
			}
			out := 0
			if outWk != nil {
				out = outWk[node]
			}
			r := want - have + out
			if r < 0 {
				r = 0
			}
			ctr := new(atomic.Int32)
			ctr.Store(int32(r))
			nodeMap[node] = ctr
		}
		remaining[wk] = nodeMap
	}

	// --------------- Activate this plan (cancel old, start TTL watcher) ---------------
	// Cancel any previous plan’s TTL context so its watcher exits.
	if old := pl.getActivePlan(); old != nil && old.Cancel != nil {
		old.Cancel()
	}
	ctxPlan, cancel := context.WithTimeout(context.Background(), PlanExecutionTimeout)
	ap := &ActivePlanState{
		ID:        id,
		PlanDoc:   sp,
		Remaining: remaining,
		Ctx:       ctxPlan,
		Cancel:    cancel,
	}
	pl.ActivePlan.Store(ap)
	// Start timeout watcher for *this* plan only.
	go pl.watchPlanTimeout(ap)
}

// ------------- Solver Helpers --------------

// fillNodesAndPods adds nodes/pods using SharedInformerFactory listers.
// If batched != nil, pending batched pods are appended with where="" (and preemptor can be nil).
func (pl *MyCrossNodePreemption) fillNodesAndPods(
	in *SolverInput,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	includePending bool,
) error {
	// Nodes
	nodes, err := pl.getNodes()
	if err != nil {
		return fmt.Errorf("list nodes (factory): %w", err)
	}
	usable := map[string]bool{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name:     n.Name,
			CPUm:     n.Status.Allocatable.Cpu().MilliValue(),
			MemBytes: n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}
	// Pods
	allPods, err := pl.getPods()
	if err != nil {
		return fmt.Errorf("list pods (factory): %w", err)
	}
	preUID := ""
	if preemptor != nil {
		preUID = string(preemptor.UID)
	}
	seen := make(map[string]bool, len(allPods)+len(batched))
	for _, p := range allPods {
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
		sp := toSolverPod(p, where)
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
			sp := toSolverPod(p, "")
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
func (pl *MyCrossNodePreemption) buildSolverInput(mode SolveMode, preemptor *v1.Pod, batched []*v1.Pod, timeout time.Duration) (SolverInput, error) {
	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		LogProgress:    SolverLogProgress,
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
		Mode:           SolverMode,
	}
	switch mode {
	case SolveSingle:
		if preemptor == nil {
			return SolverInput{}, fmt.Errorf("SolveSingle requires preemptor")
		}
		pre := toSolverPod(preemptor, "")
		in.Preemptor = &pre
		if err := pl.fillNodesAndPods(&in, preemptor, nil, false); err != nil {
			return SolverInput{}, fmt.Errorf("fill (single): %w", err)
		}
	case SolveBatch:
		if err := pl.fillNodesAndPods(&in, nil, batched, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (batch): %w", err)
		}
	case SolveContinuously:
		if err := pl.fillNodesAndPods(&in, nil, nil, true); err != nil {
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

// toSolverPod converts a Pod to a SolverPod.
func toSolverPod(p *v1.Pod, where string) SolverPod {
	return SolverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU_m:     getPodCPURequest(p),
		MemBytes:  getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
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

// IsImprovement: (1) more placed per priority (lexi), then (2) fewer evictions, then (3) fewer moves.
func IsImprovement(baseline Score, suggested Score) bool {
	// Compare placed pods in terms of priority
	if cmp := comparePlaced(suggested.PlacedByPriority, baseline.PlacedByPriority); cmp != 0 {
		klog.V(V2).InfoS("Improvement by placed pods", "cmp", cmp, "score", suggested.PlacedByPriority, "baseline", baseline.PlacedByPriority)
		return cmp > 0
	}
	// Number of evictions check
	if suggested.Evicted > baseline.Evicted {
		klog.V(V2).InfoS("No improvement: more evictions than baseline")
		return false
	}
	// Number of moves check
	if suggested.Moved > baseline.Moved {
		klog.V(V2).InfoS("No improvement: more moves than baseline")
		return false
	}
	return true // suggested is better than baseline in terms of all metrics
}

// buildInputAndBaseline builds the exact snapshot we send to the solver,
// and returns the baseline and a digest for concurrency checks.
func (pl *MyCrossNodePreemption) buildInputAndBaseline(
	mode SolveMode,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	timeout time.Duration,
) (SolverInput, Score, string, error) {
	in, err := pl.buildSolverInput(mode, preemptor, batched, timeout)
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
		if sp.Where == "" {
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
		h.Write([]byte(strconv.FormatInt(n.CPUm, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(n.MemBytes, 10)))
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
		h.Write([]byte(strconv.FormatInt(p.CPU_m, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(p.MemBytes, 10)))
		h.Write([]byte("|"))
		h.Write([]byte(strconv.FormatInt(int64(p.Priority), 10)))
		h.Write([]byte("|"))
		h.Write([]byte(p.Where))
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
func (pl *MyCrossNodePreemption) countNewAndTotalPods(out *SolverOutput) (pendingScheduled, totalPrePlan, totalPostPlan int) {
	if out == nil {
		return 0, 0, 0
	}
	totalPrePlan = 0
	pendingScheduled = 0
	totalPostPlan = 0
	pods, err := pl.getPods()
	if err != nil {
		return 0, 0, 0
	}
	podsByUID := make(map[string]*v1.Pod, len(pods))
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		podsByUID[string(p.UID)] = p
		if p.Spec.NodeName != "" {
			totalPrePlan++
		}
	}
	// Count pods that will be evicted
	evicted := 0
	for _, e := range out.Evictions {
		if p := podsByUID[e.UID]; p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName != "" {
			evicted++
		}
	}
	// Count pending pods that will be placed
	for uid, node := range out.Placements {
		if node == "" {
			continue
		}
		p := podsByUID[uid]
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName == "" { // pending -> will become running
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
