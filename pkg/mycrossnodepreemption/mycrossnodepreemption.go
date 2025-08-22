// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.0.0"

	PlanExecutionTTL = 1 * time.Minute // how long a plan may run before being terminated

	ConfigMapNamespace  = "kube-system"
	ConfigMapLabelKey   = "crossnode-plan"
	ConfigMapNamePrefix = "crossnode-plan-" // used to find plan CMs

	EvictionPollTimeout  = 30 * time.Second
	EvictionPollInterval = 1 * time.Second

	PythonSolverPath    = "/opt/solver/main.py"
	PythonSolverTimeout = 120 * time.Second

	DeletionCostTarget = math.MinInt32
	DeletionCostKeep   = math.MaxInt32
)

// ---------------------------- Plugin wiring -----------------------
func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	if obj != nil {
		klog.V(2).InfoS("Plugin configuration", "config", obj)
	}
	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}
	klog.InfoS("Plugin initialized", "name", Name, "version", Version)
	return &MyCrossNodePreemption{handle: h, client: client, blocked: newBlockedSet()}, nil
}

// ---------------------------- Types -----------------------

type MyCrossNodePreemption struct {
	handle       framework.Handle
	client       kubernetes.Interface
	activePlan   atomic.Value              // stores *StoredPlan or nil
	activePlanID atomic.Value              // string (e.g., cmName or a UUID)
	slotsPtr     atomic.Pointer[planSlots] // atomic planSlots pointer
	blocked      *blockedSet
}

type WorkloadKind int

// Deployment -> ReplicaSet
// CronJob -> Job
const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

type WorkloadKey struct {
	kind WorkloadKind
	ns   string
	name string
}

type StoredPlan struct {
	Completed              bool                      `json:"completed"`
	CompletedAt            *time.Time                `json:"completedAt,omitempty"`
	GeneratedAt            time.Time                 `json:"generatedAt"`
	PluginVersion          string                    `json:"pluginVersion"`
	PendingPod             string                    `json:"pendingPod"` // ns/name
	PendingUID             string                    `json:"pendingUID"`
	TargetNode             string                    `json:"targetNode"`
	SolverOutput           *SolverOutput             `json:"solverOutput,omitempty"`
	Plan                   PodAssignmentPlanLite     `json:"plan"`
	PlacementsByName       map[string]string         `json:"placementsByName,omitempty"`       // standalone pods -> node
	WorkloadDesiredPerNode map[string]map[string]int `json:"workloadDesiredPerNode,omitempty"` // "<ns>/<rs>" -> node -> count
}

type PodAssignmentPlanLite struct {
	TargetNode string         `json:"targetNode"`
	Movements  []MovementLite `json:"movements"`
	Evictions  []PodRefLite   `json:"evictions"`
}
type MovementLite struct {
	Pod      PodRefLite `json:"pod"`
	FromNode string     `json:"fromNode"`
	ToNode   string     `json:"toNode"`
	CPUm     int64      `json:"cpu_m"`
	MemBytes int64      `json:"mem_bytes"`
}
type PodRefLite struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type PodAssignmentPlan struct {
	TargetNode     string
	PodMovements   []PodMovement
	VictimsToEvict []*v1.Pod
}
type PodMovement struct {
	Pod           *v1.Pod
	FromNode      string
	ToNode        string
	CPURequest    int64 // milliCPU
	MemoryRequest int64 // bytes
}

type SolverNode struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"` // milliCPU
	RAM    int64             `json:"ram"` // bytes
	Labels map[string]string `json:"labels,omitempty"`
}
type SolverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	RAM       int64  `json:"ram"`
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`
	Protected bool   `json:"protected,omitempty"`
}
type SolverInput struct {
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Preemptor      SolverPod    `json:"preemptor"`
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
}
type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode"`
	Placements    map[string]string `json:"placements"` // uid -> node
	Movements     map[string]string `json:"movements"`  // optional
	Evictions     []SolverEviction  `json:"evictions"`
}

type rsNodeCounters map[string]map[string]*atomic.Int32 // rsKey -> node -> remaining

type planSlots struct {
	planID    string
	remaining rsNodeCounters
}

func (pl *MyCrossNodePreemption) startPlanTimeout(ctx context.Context, planID, cmName string, ttl time.Duration) {
	go func() {
		t := time.NewTimer(ttl)
		defer t.Stop()
		<-t.C

		// Only clear if this plan is still the active one.
		_, id := pl.getActivePlan()
		if id != planID {
			return
		}

		klog.InfoS("plan timeout reached; deactivating plan", "planID", planID, "ttl", ttl)
		pl.clearActivePlan()
	}()
}

// ---------------------------- Plan helpers (shared) -----------------------

// TODO: Only store needed ones in atomic plan
func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string) {
	pl.activePlan.Store(sp)
	pl.activePlanID.Store(id)

	// 1) Start from desired per-node targets
	desired := map[string]map[string]int{}
	for rs, perNode := range sp.WorkloadDesiredPerNode {
		desired[rs] = map[string]int{}
		for n, want := range perNode {
			desired[rs][n] = want
		}
	}

	// 2) Build a snapshot: current counts per RS/node, and a uid->pod map
	cur := map[string]map[string]int{} // rsKey -> node -> count
	podsByUID := map[string]*v1.Pod{}
	if l := pl.handle.SnapshotSharedLister(); l != nil {
		if nodes, err := l.NodeInfos().List(); err == nil {
			for _, ni := range nodes {
				if ni.Node() == nil {
					continue
				}
				for _, pi := range ni.Pods {
					p := pi.Pod
					podsByUID[string(p.UID)] = p
					if wk, ok := topWorkload(p); ok && p.Spec.NodeName != "" {
						key := wk.String()
						if _, tracked := desired[key]; tracked {
							if _, ok := cur[key]; !ok {
								cur[key] = map[string]int{}
							}
							cur[key][p.Spec.NodeName]++
						}
					}
				}
			}
		}
	}

	// 3) Count planned OUT (leaving) RS pods per node from the lite plan.
	//    We only add plannedOut for RS/node pairs that exist in desired,
	//    because nodes not in desired shouldn't get new placements.
	plannedOut := map[string]map[string]int{} // rsKey -> node -> count
	addOut := func(uid string) {
		if p := podsByUID[uid]; p != nil {
			if wk, ok := topWorkload(p); ok && p.Spec.NodeName != "" {
				key := wk.String()
				if _, tracked := desired[key]; !tracked {
					return // we don't steer this RS via quotas
				}
				if _, ok := plannedOut[key]; !ok {
					plannedOut[key] = map[string]int{}
				}
				plannedOut[key][p.Spec.NodeName]++
			}
		}
	}
	// Movements and Evictions indicate deletions of the original pods
	for _, mv := range sp.Plan.Movements {
		addOut(mv.Pod.UID)
	}
	for _, ev := range sp.Plan.Evictions {
		addOut(ev.UID)
	}

	// 4) Compute remaining = desired - current + plannedOut (clamped at >=0)
	rem := map[string]map[string]int{}
	for rs, perNode := range desired {
		rem[rs] = map[string]int{}
		for node, want := range perNode {
			have := 0
			if cur[rs] != nil {
				have = cur[rs][node]
			}
			out := 0
			if plannedOut[rs] != nil {
				out = plannedOut[rs][node]
			}
			r := want - have + out
			if r < 0 {
				r = 0
			}
			rem[rs][node] = r
		}
	}

	// 5) Convert to atomics
	counters := make(rsNodeCounters, len(rem))
	for rs, byNode := range rem {
		inner := make(map[string]*atomic.Int32, len(byNode))
		for node, v := range byNode {
			ctr := new(atomic.Int32)
			ctr.Store(int32(v))
			inner[node] = ctr
		}
		counters[rs] = inner
	}
	ps := &planSlots{planID: id, remaining: counters}
	pl.slotsPtr.Store(ps)
}

func (pl *MyCrossNodePreemption) clearActivePlan() {
	pl.activePlan.Store((*StoredPlan)(nil))
	pl.activePlanID.Store("")
	pl.slotsPtr.Store(nil)
	pl.nudgeBlockedPods()
}

func (pl *MyCrossNodePreemption) nudgeBlockedPods() error {
	nudgeBlockedPodsHelper := func(ns, name string) error {
		patch := []byte(fmt.Sprintf(
			`{"metadata":{"annotations":{"crossnode-plan/wakeup":"%d"}}}`,
			time.Now().UnixNano(),
		))
		_, err := pl.client.CoreV1().Pods(ns).Patch(
			context.TODO(),
			name,
			types.StrategicMergePatchType,
			patch,
			metav1.PatchOptions{FieldManager: "my-crossnode-plugin"},
		)
		return err
	}

	// Wakeup blocked pods by patching an annotation.
	if pl.blocked != nil {
		for uid, bi := range pl.blocked.snapshot() {
			if err := nudgeBlockedPodsHelper(bi.ns, bi.name); err != nil {
				klog.ErrorS(err, "failed to nudge blocked pod", "uid", string(uid), "pod", bi.ns+"/"+bi.name)
			}
		}
		pl.blocked.clear()
	}
	return nil
}

func (pl *MyCrossNodePreemption) getActivePlan() (*StoredPlan, string) {
	v := pl.activePlan.Load()
	if v == nil {
		return nil, ""
	}
	return v.(*StoredPlan), pl.activePlanID.Load().(string)
}

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(ctx context.Context) ([]v1.ConfigMap, error) {
	lst, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", ConfigMapLabelKey, "true"),
		},
	)
	if err != nil {
		return nil, err
	}
	// Sort newest first
	sort.Slice(lst.Items, func(i, j int) bool {
		return lst.Items[i].CreationTimestamp.Time.After(lst.Items[j].CreationTimestamp.Time)
	})
	return lst.Items, nil
}

// markPlanCompleted sets Completed=true in json (i.e. not active plan).
// Keeps the CM but prunes old history.
func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || cm == nil {
			return nil // already gone
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
		if !sp.Completed {
			now := time.Now().UTC()
			sp.Completed = true
			sp.CompletedAt = &now
			b, _ := json.MarshalIndent(&sp, "", "  ")
			patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
			_, err = pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).
				Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		}
		return nil
	})

	// Prune history (JSON-only semantics)
	if err := pl.pruneOldPlans(context.Background(), 20); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
}

// pruneOldPlans prunes old plans, keeping only the most recent 'keep' plans.
// Prefer not to follow parent context for proper cleaning, e.g. context.Background()
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

	// Find newest incomplete
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

	// Keep set = newest 'keep', force-include newest incomplete (if outside keep)
	keepSet := make(map[string]struct{}, keep)
	for i := 0; i < len(items) && len(keepSet) < keep; i++ {
		keepSet[items[i].Name] = struct{}{}
	}
	if latestIncomplete != "" {
		if _, ok := keepSet[latestIncomplete]; !ok {
			// evict the oldest among currently-kept to make room
			for i := keep - 1; i >= 0 && i < len(items); i-- {
				if _, ok := keepSet[items[i].Name]; ok && items[i].Name != latestIncomplete {
					delete(keepSet, items[i].Name)
					break
				}
			}
			keepSet[latestIncomplete] = struct{}{}
		}
	}

	// Delete the rest
	for i := range items {
		name := items[i].Name
		if _, ok := keepSet[name]; ok {
			continue
		}
		if err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete old plan ConfigMap", "configMap", name)
		}
	}
	return nil
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, sp *StoredPlan) (bool, error) {
	// A) Pending/preemptor pod must be bound to the target node if it still exists.
	pns, pname := splitNSName(sp.PendingPod)
	preemptor, err := pl.client.CoreV1().Pods(pns).Get(ctx, pname, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get pending pod: %w", err)
	}
	if err == nil {
		if preemptor.Spec.NodeName != sp.TargetNode {
			return false, nil
		}
	}

	// B) Standalone/name-addressed pods must sit on their specified node.
	for name, node := range sp.PlacementsByName {
		ns := nsOf(sp.PendingPod)
		if strings.Contains(name, "/") {
			ns, name = splitNSName(name)
		}
		pod, err := pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
		}
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName != node {
			return false, nil
		}
	}

	// C) Per-workload per-node quotas satisfied (works for RS/SS/DS/Job).
	//
	// sp.WorkloadDesiredPerNode is expected to be keyed by a stable workload key string
	// (e.g., the same format returned by WorkloadKey.String()), and values are the
	// desired replica counts per node for that workload.
	for wkStr, perNode := range sp.WorkloadDesiredPerNode {
		wk, ok := parseWorkloadKey(wkStr) // parser that mirrors WorkloadKey.String()
		if !ok {
			// If we cannot parse the workload key, we can't assert completion.
			return false, nil
		}

		// Build a label selector for this workload (controller-agnostic).
		lblSel, err := selectorForWorkload(ctx, pl.client, wk)
		if err != nil {
			return false, fmt.Errorf("selector for %s: %w", wk.String(), err)
		}
		selector, err := metav1.LabelSelectorAsSelector(&lblSel)
		if err != nil {
			return false, fmt.Errorf("build selector for %s: %w", wk.String(), err)
		}

		// List all pods that belong to the workload in its namespace.
		podList, err := pl.client.CoreV1().Pods(wk.ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return false, fmt.Errorf("list pods for %s: %w", wk.String(), err)
		}

		// Count scheduled, non-terminating pods that truly belong to this workload.
		counts := map[string]int{}
		for i := range podList.Items {
			p := &podList.Items[i]
			if p.DeletionTimestamp != nil {
				continue // ignore terminating instances
			}
			if p.Spec.NodeName == "" {
				continue // not yet scheduled; don't count
			}
			if twk, ok := topWorkload(p); !ok || !workloadEqual(twk, wk) {
				// Defensive: label match but not the same controller at runtime.
				continue
			}
			counts[p.Spec.NodeName]++
		}

		// Verify per-node desired counts for the workload.
		for node, want := range perNode {
			if counts[node] != want {
				return false, nil
			}
		}
	}

	return true, nil
}

// ---------------------------- Utilities (shared) -----------------------------

func nsOf(nsSlashName string) string {
	if i := strings.IndexByte(nsSlashName, '/'); i >= 0 {
		return nsSlashName[:i]
	}
	return "default"
}

func splitNSName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
}

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

func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
}

type blockedInfo struct {
	ns    string
	name  string
	until time.Time
}

type blockedSet struct {
	mu sync.RWMutex
	m  map[types.UID]blockedInfo
}

func newBlockedSet() *blockedSet { return &blockedSet{m: map[types.UID]blockedInfo{}} }

// Add a pod to the blocked set for 5 minutes.
func (s *blockedSet) add(uid types.UID, ns, name string) {
	s.mu.Lock()
	s.m[uid] = blockedInfo{
		ns:    ns,
		name:  name,
		until: time.Now().Add(5 * time.Minute),
	}
	s.mu.Unlock()
}

// Has returns true if the uid is still blocked (and not expired yet).
func (s *blockedSet) has(uid types.UID) bool {
	s.mu.RLock()
	bi, ok := s.m[uid]
	s.mu.RUnlock()
	return ok && time.Now().Before(bi.until)
}

// snapshot returns a stable copy of entries.
func (s *blockedSet) snapshot() map[types.UID]blockedInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[types.UID]blockedInfo, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

func (s *blockedSet) clear() {
	s.mu.Lock()
	s.m = map[types.UID]blockedInfo{}
	s.mu.Unlock()
}

func isNodeUsable(ni *framework.NodeInfo) bool {
	if ni == nil || ni.Node() == nil {
		return false
	}
	n := ni.Node()
	isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"

	return !isCP &&
		!n.Spec.Unschedulable &&
		ni.Allocatable.MilliCPU > 0 &&
		ni.Allocatable.Memory > 0
}

func (pl *MyCrossNodePreemption) materializePlanDocs(
	plan *PodAssignmentPlan,
	out *SolverOutput,
	pending *v1.Pod,
) (PodAssignmentPlanLite, map[string]string, map[string]map[string]int, error) {

	// 1) Build the lite plan from the concrete plan we execute.
	lite := PodAssignmentPlanLite{TargetNode: plan.TargetNode}
	for _, mv := range plan.PodMovements {
		lite.Movements = append(lite.Movements, MovementLite{
			Pod:      PodRefLite{Namespace: mv.Pod.Namespace, Name: mv.Pod.Name, UID: string(mv.Pod.UID)},
			FromNode: mv.FromNode,
			ToNode:   mv.ToNode,
			CPUm:     mv.CPURequest,
			MemBytes: mv.MemoryRequest,
		})
	}
	for _, v := range plan.VictimsToEvict {
		lite.Evictions = append(lite.Evictions, PodRefLite{
			Namespace: v.Namespace, Name: v.Name, UID: string(v.UID),
		})
	}

	// 2) Map uid -> *Pod from current snapshot (+ pending)
	podsByUID := map[string]*v1.Pod{}
	all, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return PodAssignmentPlanLite{}, nil, nil, err
	}
	for _, ni := range all {
		for _, pi := range ni.Pods {
			podsByUID[string(pi.Pod.UID)] = pi.Pod
		}
	}
	podsByUID[string(pending.UID)] = pending

	// 3) From solver placements: build byName + rsDesired
	byName := make(map[string]string)
	rsDesired := map[string]map[string]int{}

	for uid, node := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || p == nil {
			continue
		}
		// Skip the pending pod – its placement is enforced by UID pinning, not RS counters
		if uid == string(pending.UID) {
			continue
		}

		if wk, ok := topWorkload(p); ok {
			key := wk.String()
			if _, ok := rsDesired[key]; !ok {
				rsDesired[key] = map[string]int{}
			}
			rsDesired[key][node]++
		} else {
			byName[p.Namespace+"/"+p.Name] = node // use ns/name (see below)
		}
	}

	return lite, byName, rsDesired, nil
}

func (wk WorkloadKey) String() string {
	switch wk.kind {
	case wkReplicaSet:
		return "rs:" + wk.ns + "/" + wk.name
	case wkStatefulSet:
		return "ss:" + wk.ns + "/" + wk.name
	case wkDaemonSet:
		return "ds:" + wk.ns + "/" + wk.name
	case wkJob:
		return "job:" + wk.ns + "/" + wk.name
	default:
		return wk.ns + "/" + wk.name
	}
}

// topWorkload returns the controlling workload for a Pod.
// Deployment is represented by its current ReplicaSet; CronJob is represented by the Job.
func topWorkload(p *v1.Pod) (WorkloadKey, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		switch o.Kind {
		case "ReplicaSet":
			return WorkloadKey{kind: wkReplicaSet, ns: p.Namespace, name: o.Name}, true
		case "StatefulSet":
			return WorkloadKey{kind: wkStatefulSet, ns: p.Namespace, name: o.Name}, true
		case "DaemonSet":
			return WorkloadKey{kind: wkDaemonSet, ns: p.Namespace, name: o.Name}, true
		case "Job":
			return WorkloadKey{kind: wkJob, ns: p.Namespace, name: o.Name}, true
		}
	}
	return WorkloadKey{}, false
}

func workloadEqual(a, b WorkloadKey) bool {
	return a.kind == b.kind && a.ns == b.ns && a.name == b.name
}

// selectorForWorkload returns a LabelSelector for all Pods that belong to the workload.
func selectorForWorkload(ctx context.Context, cli kubernetes.Interface, wk WorkloadKey) (metav1.LabelSelector, error) {
	switch wk.kind {
	case wkReplicaSet:
		rs, err := cli.AppsV1().ReplicaSets(wk.ns).Get(ctx, wk.name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *rs.Spec.Selector, nil

	case wkStatefulSet:
		ss, err := cli.AppsV1().StatefulSets(wk.ns).Get(ctx, wk.name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ss.Spec.Selector, nil

	case wkDaemonSet:
		ds, err := cli.AppsV1().DaemonSets(wk.ns).Get(ctx, wk.name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ds.Spec.Selector, nil

	case wkJob:
		job, err := cli.BatchV1().Jobs(wk.ns).Get(ctx, wk.name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		// Prefer explicit selector if present; otherwise use template labels.
		if job.Spec.Selector != nil {
			return *job.Spec.Selector, nil
		}
		return metav1.LabelSelector{MatchLabels: job.Spec.Template.Labels}, nil

	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind for selector: %v", wk.kind)
	}
}

// parseWorkloadKey parses the string form produced by WorkloadKey.String().
// Example formats you might use:
//
//	"rs:ns/name", "ss:ns/name", "ds:ns/name", "job:ns/name"
func parseWorkloadKey(s string) (WorkloadKey, bool) {
	// kind:ns/name
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return WorkloadKey{}, false
	}
	kindStr, rest := s[:colon], s[colon+1:]
	ns, name := splitNSName(rest)

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
	return WorkloadKey{kind: k, ns: ns, name: name}, true
}
