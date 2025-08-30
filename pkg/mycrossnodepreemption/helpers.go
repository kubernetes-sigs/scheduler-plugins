// helpers.go

package mycrossnodepreemption

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// ------ Batch Helpers -------
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

// ----- Strategy Helpers -------

// Optimizer cadence (per-preemptor vs. batches)
func optimizeForEvery() bool     { return OptimizeCadence == OptimizeForEvery }
func optimizeInBatches() bool    { return OptimizeCadence == OptimizeInBatches }
func optimizeContinuously() bool { return OptimizeCadence == OptimizeContinuously }

// Action point (PreEnqueue vs. PostFilter)
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

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

func (pl *MyCrossNodePreemption) decideStrategy(phase Phase) StrategyDecision {
	// Continuous mode never blocks or batches due to the optimizer.
	if optimizeContinuously() {
		return DecidePassThrough
	}
	if pl.Active.Load() {
		return DecideBlockActive
	}
	if optimizeInBatches() {
		if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
			return DecideBatch
		}
		return DecidePassThrough
	}
	if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
		return DecideEvery
	}
	return DecidePassThrough
}

// ---------- Helpers for objects --------------

func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func splitNamespaceName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
}

func (pl *MyCrossNodePreemption) getNodes() ([]*v1.Node, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.Handle.SharedInformerFactory().Core().V1().Nodes().Lister().List(labels.Everything())
}

func (pl *MyCrossNodePreemption) getPods() ([]*v1.Pod, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister().List(labels.Everything())
}

// --------- Pod specifications functions ---------

func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Cpu().MilliValue()
	}
	return total
}

func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
	}
	return total
}

func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}

func isControlPlane(n *v1.Node) bool {
	return n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
}

func nodeReady(n *v1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

func hasNoScheduleCondTaint(n *v1.Node) bool {
	for _, t := range n.Spec.Taints {
		if (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") &&
			(t.Effect == v1.TaintEffectNoSchedule || string(t.Effect) == "") {
			return true
		}
	}
	return false
}

func isNodeUsable(n *v1.Node) bool {
	if n == nil || isControlPlane(n) || n.Spec.Unschedulable {
		return false
	}
	if !nodeReady(n) {
		// This is what becomes node.kubernetes.io/not-ready in scheduling.
		return false
	}
	if hasNoScheduleCondTaint(n) {
		return false
	}
	return n.Status.Allocatable.Cpu().MilliValue() > 0 &&
		n.Status.Allocatable.Memory().Value() > 0
}

// --------- Pod set functions ---------

func newPodSet() *PodSet { return &PodSet{m: make(map[types.UID]PodKey)} }

func (s *PodSet) AddRef(uid types.UID, ns, name string) {
	s.mu.Lock()
	s.m[uid] = PodKey{UID: uid, Namespace: ns, Name: name}
	s.mu.Unlock()
}

func (s *PodSet) AddPod(p *v1.Pod) {
	if p == nil {
		return
	}
	s.AddRef(p.UID, p.Namespace, p.Name)
}

func (s *PodSet) Remove(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

func (s *PodSet) Clear() {
	s.mu.Lock()
	s.m = make(map[types.UID]PodKey)
	s.mu.Unlock()
}

func (s *PodSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

func (s *PodSet) Snapshot() map[types.UID]PodKey {
	s.mu.RLock()
	out := make(map[types.UID]PodKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}

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
		case apierrors.IsNotFound(err):
			set.Remove(uid)
			removed++
		case err != nil:
			// Conservative; keep if lister errored
		default:
			// Drop if recreated/terminating
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil {
				set.Remove(uid)
				removed++
				continue
			}
			// Apply caller-specific predicate
			if keep != nil && !keep(cur) {
				set.Remove(uid)
				removed++
			}
		}
	}
	return removed
}

// Activate up to 'max' pods from the blocked set; clear only the ones activated.
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
		klog.V(V2).InfoS("Activated blocked pods", "count", len(toAct), "max", max)

		// Remove only the activated ones from the set
		for _, it := range items[:limit] {
			pl.Blocked.Remove(it.key.UID)
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
			pl.Batched.Remove(it.key.UID)
		}
	}

	// If caller passes podsToRemove; remove them from the batched set
	for _, p := range podsToRemove {
		if p != nil {
			pl.Batched.Remove(p.UID)
		}
	}
}

func (pl *MyCrossNodePreemption) pruneStaleSetEntries(set *PodSet) int {
	rem := pl.pruneSetStale(set, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep function: keep only pending pods
	})
	if rem > 0 {
		klog.V(V2).InfoS("Pruned stale entries", "removed", rem)
	}
	return rem
}

// ---------- Helpers for Workloads --------------

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

func workloadEqual(a, b WorkloadKey) bool {
	return a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

func workloadSelector(ctx context.Context, cli kubernetes.Interface, wk WorkloadKey) (metav1.LabelSelector, error) {
	switch wk.Kind {
	case wkReplicaSet:
		rs, err := cli.AppsV1().ReplicaSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *rs.Spec.Selector, nil
	case wkStatefulSet:
		ss, err := cli.AppsV1().StatefulSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ss.Spec.Selector, nil
	case wkDaemonSet:
		ds, err := cli.AppsV1().DaemonSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ds.Spec.Selector, nil
	case wkJob:
		job, err := cli.BatchV1().Jobs(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		if job.Spec.Selector != nil {
			return *job.Spec.Selector, nil
		}
		return metav1.LabelSelector{MatchLabels: job.Spec.Template.Labels}, nil
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind for selector: %v", wk.Kind)
	}
}

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

// -------------- Other utility functions --------------

func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
}

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

func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
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

func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	// Use context's deadline if any, else default to 2m
	// TODO: make this better, remove client calls and improve time settings
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}
		for k := range remaining {
			got, err := pl.Client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				delete(remaining, k)
				continue
			}
			if err != nil {
				// transient error: keep polling
				return false, nil
			}
			if string(got.UID) != k.uid {
				// name reused; original is gone
				delete(remaining, k)
			}
		}
		if len(remaining) == 0 {
			return true, nil
		}
		klog.V(V2).InfoS("Waiting for targeted pods to disappear", "remaining", len(remaining))
		return false, nil
	})
}

// ---------- Helpers for Plan ------------

func (pl *MyCrossNodePreemption) tryEnterActive() bool {
	return pl.Active.CompareAndSwap(false, true)
}

func (pl *MyCrossNodePreemption) leaveActive() {
	pl.Active.Store(false)
}

func (pl *MyCrossNodePreemption) executePlanIfOps(ctx context.Context, plan *Plan) bool {
	if plan == nil {
		return false
	}
	if len(plan.Moves) == 0 && len(plan.Evicts) == 0 {
		return false
	}
	if err := pl.executePlan(ctx, plan); err != nil {
		klog.ErrorS(err, "Plan execution failed")
		pl.onPlanSettled()
		return false
	}
	return true
}

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

// ------------- Solver Helpers --------------
func solverFeasible(out *SolverOutput) bool {
	return out != nil && (out.Status == "OPTIMAL" || out.Status == "FEASIBLE")
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
	if cmp := comparePlaced(suggested.PlacedByPriority, baseline.PlacedByPriority); cmp != 0 {
		klog.V(V2).InfoS("Improvement by placed pods", "cmp", cmp, "score", suggested.PlacedByPriority, "baseline", baseline.PlacedByPriority)
		return cmp > 0
	}
	// better fallback: only treat as improvement if there are real ops
	if suggested.Evicted > baseline.Evicted {
		klog.V(V2).InfoS("No improvement: more evictions than baseline")
		return false
	}
	if suggested.Moved > baseline.Moved {
		klog.V(V2).InfoS("No improvement: more moves than baseline")
		return false
	}
	hasOps := suggested.Evicted > 0 || suggested.Moved > 0
	klog.V(V2).InfoS("Fallback: require real ops", "hasOps", hasOps)
	return hasOps
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

// countNewAndTotalPods computes, from the live cluster view and the solver output:
//
//	 pendingScheduled  = # of currently-pending pods that got a placement in this plan
//		total  = (# running now) - (# running that will be evicted) + pendingScheduled
//
// It does NOT need the batched pods list; it uses the shared informer lister.
func (pl *MyCrossNodePreemption) countNewAndTotalPods(out *SolverOutput) (pendingScheduled, total int) {
	if out == nil {
		return 0, 0
	}

	pods, err := pl.getPods()
	if err != nil {
		return 0, 0
	}
	podsByUID := make(map[string]*v1.Pod, len(pods))
	runningNow := 0
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		podsByUID[string(p.UID)] = p
		if p.Spec.NodeName != "" {
			runningNow++
		}
	}

	// 2) How many currently running will be evicted by this plan?
	evicted := 0
	for _, e := range out.Evictions {
		if p := podsByUID[e.UID]; p != nil && p.DeletionTimestamp == nil && p.Spec.NodeName != "" {
			evicted++
		}
	}

	// 3) How many currently pending get placed by this plan?
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
		// Note: moves of already-running pods don’t change the running count.
	}

	total = runningNow - evicted + pendingScheduled
	if total < 0 {
		total = 0 // safety clamp
	}
	return pendingScheduled, total
}
