// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
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
	Name                   = "MyCrossNodePreemption"
	Version                = "v1.0.4"
	DeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

	ConfigMapNamespace  = "kube-system"
	ConfigMapLabelKey   = "crossnode-plan"
	ConfigMapNamePrefix = "crossnode-plan-" // used to find plan CMs

	PollTimeout  = 60 * time.Second
	PollInterval = 1 * time.Second

	PythonSolverPath    = "/opt/solver/main.py"
	PythonSolverTimeout = 80 * time.Second

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
	return &MyCrossNodePreemption{handle: h, client: client}, nil
}

// ---------------------------- Types -----------------------
type MyCrossNodePreemption struct {
	handle       framework.Handle
	client       kubernetes.Interface
	activePlan   atomic.Value              // stores *StoredPlan or nil
	activePlanID atomic.Value              // string (e.g., cmName or a UUID)
	slotsPtr     atomic.Pointer[planSlots] // atomic planSlots pointer
}

type StoredPlan struct {
	Completed        bool                      `json:"completed"`
	CompletedAt      *time.Time                `json:"completedAt,omitempty"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	PluginVersion    string                    `json:"pluginVersion"`
	PendingPod       string                    `json:"pendingPod"` // ns/name
	PendingUID       string                    `json:"pendingUID"`
	TargetNode       string                    `json:"targetNode"`
	SolverOutput     *SolverOutput             `json:"solverOutput,omitempty"`
	Plan             PodAssignmentPlanLite     `json:"plan"`
	PlacementsByName map[string]string         `json:"placementsByName,omitempty"` // standalone pods -> node
	RSDesiredPerNode map[string]map[string]int `json:"rsDesiredPerNode,omitempty"` // "<ns>/<rs>" -> node -> count
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

// ---------------------------- Plan helpers (shared) -----------------------

// TODO: Only store needed ones in atomic plan
func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string) {
	pl.activePlan.Store(sp)
	pl.activePlanID.Store(id)

	// 1) Start from desired per-node targets
	desired := map[string]map[string]int{}
	for rs, perNode := range sp.RSDesiredPerNode {
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
					if rsName, ok := owningRS(p); ok && p.Spec.NodeName != "" {
						rk := rsKey(p.Namespace, rsName)
						if _, tracked := desired[rk]; tracked {
							if _, ok := cur[rk]; !ok {
								cur[rk] = map[string]int{}
							}
							cur[rk][p.Spec.NodeName]++
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
			if rsName, ok := owningRS(p); ok && p.Spec.NodeName != "" {
				rk := rsKey(p.Namespace, rsName)
				if _, tracked := desired[rk]; !tracked {
					return // we don't steer this RS via quotas
				}
				if _, ok := plannedOut[rk]; !ok {
					plannedOut[rk] = map[string]int{}
				}
				plannedOut[rk][p.Spec.NodeName]++
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
	if err := pl.pruneOldPlans(ctx, 20); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
}

// pruneOldPlans prunes old plans, keeping only the most recent 'keep' plans.
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
	// A) If the preemptor Pod (by the recorded name) still exists, it must be bound to TargetNode.
	//    This applies whether it's RS-owned or not. If NotFound, continue with B/C.
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

	// B) Standalone/name-addressed pods placed on their target node
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

	// C) RS per-node quotas satisfied
	for rsKeyStr, perNode := range sp.RSDesiredPerNode {
		ns, rsName := splitNSName(rsKeyStr)

		rs, err := pl.client.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get rs %s/%s: %w", ns, rsName, err)
		}
		sel, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return false, fmt.Errorf("selector for rs %s/%s: %w", ns, rsName, err)
		}
		podList, err := pl.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return false, fmt.Errorf("list rs pods %s/%s: %w", ns, rsName, err)
		}

		counts := map[string]int{}
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.DeletionTimestamp != nil {
				continue // ignore terminating
			}
			if r, ok := owningRS(pod); !ok || r != rsName {
				continue // make sure it's this RS
			}
			if pod.Spec.NodeName == "" {
				continue // not yet scheduled; don't count
			}
			counts[pod.Spec.NodeName]++
		}

		for node, want := range perNode {
			if counts[node] != want {
				return false, nil
			}
		}
	}

	return true, nil
}

// ---------------------------- Utilities (shared) -----------------------------

func rsKey(ns, rs string) string { return ns + "/" + rs }

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

func owningRS(p *v1.Pod) (string, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" {
			return o.Name, true
		}
	}
	return "", false
}

func isControlledByRS(p *v1.Pod, rsName string) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller &&
			o.Kind == "ReplicaSet" && o.Name == rsName {
			return true
		}
	}
	return false
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

		if rsName, okRS := owningRS(p); okRS {
			k := rsKey(p.Namespace, rsName)
			if _, ok := rsDesired[k]; !ok {
				rsDesired[k] = map[string]int{}
			}
			rsDesired[k][node]++
		} else {
			byName[p.Namespace+"/"+p.Name] = node // use ns/name (see below)
		}
	}

	return lite, byName, rsDesired, nil
}
