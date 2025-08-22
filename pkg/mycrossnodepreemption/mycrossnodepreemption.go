// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.3.1"

	// ======= Modes =======
	// If true, we collect pods in a local batch and solve every SolveInterval.
	// If false, pods flow normally and/or PostFilter can run single-preemptor solving.
	BatchModeEnabled = false

	// If true, PostFilter (when reached and BatchModeEnabled==false) will take the single
	// unschedulable preemptor and compute/execute a plan immediately.
	PostFilterSinglePreemptor = true

	// ======= Batch settings =======
	BatchSolveInterval = 30 * time.Second // periodic cohort solve

	// ======= Plan settings =======
	PlanExecutionTTL = 1 * time.Minute // how long a plan may run before being terminated

	ConfigMapNamespace = "kube-system"
	ConfigMapLabelKey  = "crossnode-plan"

	EvictionPollTimeout  = 30 * time.Second
	EvictionPollInterval = 1 * time.Second

	PythonSolverPath    = "/opt/solver/main.py"
	PythonSolverTimeout = 120 * time.Second
)

// ---------------------------- Plugin wiring -----------------------
func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// sanity: exactly one mode
	if (BatchModeEnabled && PostFilterSinglePreemptor) || (!BatchModeEnabled && !PostFilterSinglePreemptor) {
		return nil, fmt.Errorf("%s: invalid config: enable exactly one of BatchModeEnabled or PostFilterSinglePreemptor", Name)
	}

	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}
	pl := &MyCrossNodePreemption{
		handle:      h,
		client:      client,
		blockedPods: newBlockedPodsSet(),
		batchPods:   make(map[types.UID]*v1.Pod, 32),
	}
	klog.InfoS("Plugin initialized", "name", Name, "version", Version,
		"batchMode", BatchModeEnabled, "singlePreemptorMode", PostFilterSinglePreemptor)

	if BatchModeEnabled {
		go pl.batchLoop(context.Background())
	}
	return pl, nil
}

// ---------------------------- Types -----------------------

type MyCrossNodePreemption struct {
	handle       framework.Handle
	client       kubernetes.Interface
	activePlan   atomic.Value              // stores *StoredPlan or nil
	activePlanID atomic.Value              // string (e.g., cmName or a UUID)
	slotsPtr     atomic.Pointer[planSlots] // atomic planSlots pointer

	blockedPods *blockedPodsSet // holds pods we "block" while a plan executes

	// Batching (pods seen during last interval, before a plan is created)
	batchMu   sync.Mutex
	batchPods map[types.UID]*v1.Pod
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
	PendingPod             string                    `json:"pendingPod"` // ns/name (kept for compatibility; any "lead" in cohort)
	PendingUID             string                    `json:"pendingUID"`
	TargetNode             string                    `json:"targetNode"` // may be empty in batch mode
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

type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// NOTE: Nominations removed; nominatedNode is optional and used only in single-preemptor mode.
type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode,omitempty"`
	Placements    map[string]string `json:"placements"` // uid -> node
	Evictions     []SolverEviction  `json:"evictions"`
}

type rsNodeCounters map[string]map[string]*atomic.Int32 // rsKey -> node -> remaining

type planSlots struct {
	planID    string
	remaining rsNodeCounters
}

// ---------------------------- Batch loop -----------------------

func (pl *MyCrossNodePreemption) batchLoop(ctx context.Context) {
	ticker := time.NewTicker(BatchSolveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			func() {
				// bail if a plan is already running
				if sp, _ := pl.getActivePlan(); sp != nil && !sp.Completed {
					return
				}

				// Ensure the cache has at least one usable node before touching the batch.
				if !pl.haveUsableNodes() {
					klog.InfoS("Batch loop: no usable nodes yet; keeping batch intact")
					return
				}

				// Snapshot (do NOT clear yet). We only remove on success.
				pods := pl.snapshotBatch()
				if len(pods) == 0 {
					return
				}

				// run cohort solve (no explicit preemptor)
				cctx, cancel := context.WithTimeout(context.Background(), PythonSolverTimeout)
				// log solver time
				startTime := time.Now()
				out, err := pl.runPythonOptimizerCohort(cctx, pods, PythonSolverTimeout)
				klog.InfoS("batch solve completed", "duration", time.Since(startTime))
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

				lite, byName, rsDesired, err := pl.materializePlanDocs(plan, out, pending)
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
					WorkloadDesiredPerNode: rsDesired,
				}
				pl.setActivePlan(inMem, cmName)

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
				podLister := pl.handle.SharedInformerFactory().Core().V1().Pods().Lister()
				for _, p := range pods {
					lp, err := podLister.Pods(p.Namespace).Get(p.Name)
					if err == nil {
						activate[p.Namespace+"/"+p.Name] = lp
					}
				}
				if len(activate) > 0 {
					pl.handle.Activate(klog.Background(), activate)
				}

				// Now that we succeeded, remove these pods from the batch.
				pl.removeFromBatchByUIDs(pods)

				klog.InfoS("batch plan executed; batch activated", "batchSize", len(pods), "planID", planID, "moved", len(plan.PodMovements), "evicted", len(plan.VictimsToEvict))
			}()
		case <-ctx.Done():
			return
		}
	}
}

// has at least one usable node in the scheduler snapshot
func (pl *MyCrossNodePreemption) haveUsableNodes() bool {
	nodes, err := pl.getNodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if isNodeUsable(n) {
			return true
		}
	}
	return false
}

// --- batch helpers ---

// snapshotBatch returns a copy of the current batch without clearing it.
func (pl *MyCrossNodePreemption) snapshotBatch() []*v1.Pod {
	pl.batchMu.Lock()
	defer pl.batchMu.Unlock()
	if len(pl.batchPods) == 0 {
		return nil
	}
	out := make([]*v1.Pod, 0, len(pl.batchPods))
	for _, p := range pl.batchPods {
		out = append(out, p)
	}
	return out
}

// removeFromBatchByUIDs deletes the provided pods from the batch (called on success).
func (pl *MyCrossNodePreemption) removeFromBatchByUIDs(pods []*v1.Pod) {
	pl.batchMu.Lock()
	for _, p := range pods {
		delete(pl.batchPods, p.UID)
	}
	pl.batchMu.Unlock()
}

func (pl *MyCrossNodePreemption) addToBatch(p *v1.Pod) {
	pl.batchMu.Lock()
	pl.batchPods[p.UID] = p
	pl.batchMu.Unlock()
}

// ---------------------------- Plan lifecycle -----------------------

func (pl *MyCrossNodePreemption) startPlanTimeout(planID string, ttl time.Duration) {
	go func() {
		t := time.NewTimer(ttl)
		defer t.Stop()
		<-t.C

		_, id := pl.getActivePlan()
		if id != planID {
			return
		}
		klog.InfoS("plan timeout reached; deactivating plan", "planID", planID, "ttl", ttl)
		pl.onPlanSettled()
	}()
}

// TODO: Only store needed ones in atomic plan
func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string) {
	pl.activePlan.Store(sp)
	pl.activePlanID.Store(id)

	// 1) desired per-node targets
	desired := map[string]map[string]int{}
	for rs, perNode := range sp.WorkloadDesiredPerNode {
		desired[rs] = map[string]int{}
		for n, want := range perNode {
			desired[rs][n] = want
		}
	}

	// 2) current per RS/node + uid map
	cur := map[string]map[string]int{}
	podsByUID := map[string]*v1.Pod{}
	if live, err := pl.getPods(); err == nil {
		for _, p := range live {
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

	// 3) planned OUT counts from lite plan
	plannedOut := map[string]map[string]int{}
	addOut := func(uid string) {
		if p := podsByUID[uid]; p != nil {
			if wk, ok := topWorkload(p); ok && p.Spec.NodeName != "" {
				key := wk.String()
				if _, tracked := desired[key]; !tracked {
					return
				}
				if _, ok := plannedOut[key]; !ok {
					plannedOut[key] = map[string]int{}
				}
				plannedOut[key][p.Spec.NodeName]++
			}
		}
	}
	for _, mv := range sp.Plan.Movements {
		addOut(mv.Pod.UID)
	}
	for _, ev := range sp.Plan.Evictions {
		addOut(ev.UID)
	}

	// 4) remaining = desired - current + plannedOut (>=0)
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

	// 5) atomics
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
	sort.Slice(lst.Items, func(i, j int) bool {
		return lst.Items[i].CreationTimestamp.Time.After(lst.Items[j].CreationTimestamp.Time)
	})
	return lst.Items, nil
}

// markPlanCompleted sets Completed=true in json (i.e. not active plan).
func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
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

	if err := pl.pruneOldPlans(context.Background(), 20); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
}

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
		if err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete old plan ConfigMap", "configMap", name)
		}
	}
	return nil
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, sp *StoredPlan) (bool, error) {
	// A) Pending/preemptor pod bound to target node (only meaningful if TargetNode set)
	if sp.TargetNode != "" && sp.PendingPod != "" {
		pns, pname := splitNSName(sp.PendingPod)
		preemptor, err := pl.client.CoreV1().Pods(pns).Get(ctx, pname, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get pending pod: %w", err)
		}
		if err == nil && preemptor.Spec.NodeName != sp.TargetNode {
			return false, nil
		}
	}

	// B) Standalone/name-addressed pods on specified node.
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

	// C) Per-workload per-node quotas satisfied.
	for wkStr, perNode := range sp.WorkloadDesiredPerNode {
		wk, ok := parseWorkloadKey(wkStr)
		if !ok {
			return false, nil
		}
		lblSel, err := selectorForWorkload(ctx, pl.client, wk)
		if err != nil {
			return false, fmt.Errorf("selector for %s: %w", wk.String(), err)
		}
		selector, err := metav1.LabelSelectorAsSelector(&lblSel)
		if err != nil {
			return false, fmt.Errorf("build selector for %s: %w", wk.String(), err)
		}
		podList, err := pl.client.CoreV1().Pods(wk.ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return false, fmt.Errorf("list pods for %s: %w", wk.String(), err)
		}
		counts := map[string]int{}
		for i := range podList.Items {
			p := &podList.Items[i]
			if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
				continue
			}
			if twk, ok := topWorkload(p); !ok || !workloadEqual(twk, wk) {
				continue
			}
			counts[p.Spec.NodeName]++
		}
		for node, want := range perNode {
			if counts[node] != want {
				return false, nil
			}
		}
	}

	return true, nil
}

// ---------------------------- Utilities -----------------------------

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

type blockedPodInfo struct {
	ns    string
	name  string
	until time.Time
}

type blockedPodsSet struct {
	mu sync.RWMutex
	m  map[types.UID]blockedPodInfo
}

func newBlockedPodsSet() *blockedPodsSet { return &blockedPodsSet{m: map[types.UID]blockedPodInfo{}} }

func (s *blockedPodsSet) add(uid types.UID, ns, name string) {
	s.mu.Lock()
	s.m[uid] = blockedPodInfo{
		ns:    ns,
		name:  name,
		until: time.Now().Add(5 * time.Minute),
	}
	s.mu.Unlock()
}

func (s *blockedPodsSet) snapshot() map[types.UID]blockedPodInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[types.UID]blockedPodInfo, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

func (s *blockedPodsSet) clear() {
	s.mu.Lock()
	s.m = map[types.UID]blockedPodInfo{}
	s.mu.Unlock()
}

// ---------------------------- Plan docs & solver bridge ---------------------

func (pl *MyCrossNodePreemption) materializePlanDocs(
	plan *PodAssignmentPlan,
	out *SolverOutput,
	pending *v1.Pod,
) (PodAssignmentPlanLite, map[string]string, map[string]map[string]int, error) {

	// 1) lite plan
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

	// 2) uid -> pod map (+ pending)
	podsByUID := map[string]*v1.Pod{}
	live, err := pl.getPods()
	if err != nil {
		return PodAssignmentPlanLite{}, nil, nil, err
	}
	for _, p := range live {
		podsByUID[string(p.UID)] = p
	}
	podsByUID[string(pending.UID)] = pending

	// 3) build byName + rsDesired from placements
	byName := make(map[string]string)
	rsDesired := map[string]map[string]int{}

	for uid, node := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || p == nil {
			continue
		}
		// Skip the lead pending pod if any; its binding is enforced by UID pinning (single-preemptor only)
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
			byName[p.Namespace+"/"+p.Name] = node
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
		if job.Spec.Selector != nil {
			return *job.Spec.Selector, nil
		}
		return metav1.LabelSelector{MatchLabels: job.Spec.Template.Labels}, nil
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind for selector: %v", wk.kind)
	}
}

func parseWorkloadKey(s string) (WorkloadKey, bool) {
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

// ----------------------- Solver bridge (cohort) ----------------------------

// Cohort-capable runner
func (pl *MyCrossNodePreemption) runPythonOptimizerCohort(
	ctx context.Context,
	preemptors []*v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {
	// Live nodes
	nodes, err := pl.getNodes()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	type SolverInputCohort struct {
		TimeoutMs      int64        `json:"timeout_ms"`
		IgnoreAffinity bool         `json:"ignore_affinity"`
		Preemptors     []SolverPod  `json:"preemptors,omitempty"`
		Nodes          []SolverNode `json:"nodes"`
		Pods           []SolverPod  `json:"pods"`
	}
	in := SolverInputCohort{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptors:     make([]SolverPod, 0),
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
	}

	usable := map[string]bool{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name: n.Name,
			CPU:  n.Status.Allocatable.Cpu().MilliValue(),
			RAM:  n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}
	if len(in.Nodes) == 0 {
		return nil, fmt.Errorf("no usable nodes available")
	}

	// Live pods
	allPods, err := pl.getPods()
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range allPods {
		where := p.Spec.NodeName
		if where != "" && !usable[where] {
			continue
		}
		sp := toSolverPod(p, where)
		if p.Namespace == "kube-system" {
			sp.Protected = true
		}
		in.Pods = append(in.Pods, sp)
	}

	// include preemptors explicitly and also as unscheduled pods
	for _, p := range preemptors {
		in.Preemptors = append(in.Preemptors, toSolverPod(p, ""))
		in.Pods = append(in.Pods, toSolverPod(p, ""))
	}

	raw, _ := json.Marshal(in)
	klog.V(2).InfoS("Solver input", "raw", string(raw),
		"nodes", len(in.Nodes), "pods", len(in.Pods), "preemptors", len(in.Preemptors))

	cmd := exec.CommandContext(ctx, "python3", PythonSolverPath)
	cmd.Stdin = bytes.NewReader(raw)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Run(); err != nil {
		klog.ErrorS(err, "python solver failed", "stderr", errBuf.String())
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if out.Status != "OK" {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}

func toSolverPod(p *v1.Pod, where string) SolverPod {
	return SolverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU:       getPodCPURequest(p),
		RAM:       getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
	}
}

// ---------------------------- Plan translation / export / execution -----------

func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	out *SolverOutput,
	pending *v1.Pod,
) (*PodAssignmentPlan, error) {
	// In single-preemptor mode we expect NominatedNode != "".
	// In batch mode NominatedNode may be "".
	plan := &PodAssignmentPlan{TargetNode: out.NominatedNode}

	podsByUID := map[string]*v1.Pod{}
	live, err := pl.getPods()
	if err != nil {
		return nil, err
	}
	for _, p := range live {
		podsByUID[string(p.UID)] = p
	}
	podsByUID[string(pending.UID)] = pending

	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.VictimsToEvict = append(plan.VictimsToEvict, p)
		}
	}
	for uid, dest := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || uid == string(pending.UID) {
			continue
		}
		from := p.Spec.NodeName
		if from == dest || dest == "" {
			continue
		}
		plan.PodMovements = append(plan.PodMovements, PodMovement{
			Pod:           p,
			FromNode:      from,
			ToNode:        dest,
			CPURequest:    getPodCPURequest(p),
			MemoryRequest: getPodMemoryRequest(p),
		})
	}
	return plan, nil
}

func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	plan *PodAssignmentPlan,
	out *SolverOutput,
	pending *v1.Pod,
) (string, error) {
	litePlan, byName, rsDesired, err := pl.materializePlanDocs(plan, out, pending)
	if err != nil {
		return "", err
	}

	doc := &StoredPlan{
		Completed:              false,
		CompletedAt:            nil,
		GeneratedAt:            time.Now().UTC(),
		PluginVersion:          Version,
		PendingPod:             fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:             string(pending.UID),
		TargetNode:             plan.TargetNode, // may be empty in batch
		SolverOutput:           out,
		Plan:                   litePlan,
		PlacementsByName:       byName,
		WorkloadDesiredPerNode: rsDesired,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("crossnode-plan-%s-%d", pending.UID, time.Now().Unix())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ConfigMapNamespace,
			Labels:    map[string]string{ConfigMapLabelKey: "true"},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}

	_ = pl.pruneOldPlans(ctx, 20)
	return name, nil
}

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan) error {
	// Collect unique target pods (moves + evictions).
	var targets []*v1.Pod
	seen := map[string]bool{}
	for _, mv := range plan.PodMovements {
		key := mv.Pod.Namespace + "/" + mv.Pod.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, mv.Pod)
		}
	}
	for _, v := range plan.VictimsToEvict {
		key := v.Namespace + "/" + v.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, v)
		}
	}

	// 1) evict/wait pods
	if len(targets) > 0 {
		klog.V(2).InfoS("Evicting/awaiting eviction of targeted pods", "count", len(targets))

		for _, p := range targets {
			if err := pl.evictPod(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict pod %s: %w", podRef(p), err)
			}
		}

		// Wait for all targeted pods to actually disappear
		if err := pl.waitPodsGone(ctx, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// 2) recreate standalone pods (no bind)
	for _, mv := range plan.PodMovements {
		if _, ok := topWorkload(mv.Pod); ok {
			klog.V(2).InfoS("Skipping workload-owned move recreate (controller will recreate)", "pod", podRef(mv.Pod), "to", mv.ToNode)
			continue
		}
		klog.V(2).InfoS("Recreating moved standalone pod (no bind)", "pod", podRef(mv.Pod))
		if err := pl.recreatePod(ctx, mv.Pod, ""); err != nil {
			return fmt.Errorf("recreate moved pod %s: %w", podRef(mv.Pod), err)
		}
	}
	for _, v := range plan.VictimsToEvict {
		if _, ok := topWorkload(v); ok {
			klog.V(2).InfoS("Skipping workload-owned eviction recreate (controller will recreate)", "pod", podRef(v))
			continue
		}
		klog.V(2).InfoS("Recreating evicted standalone pod (no bind)", "pod", podRef(v))
		if err := pl.recreatePod(ctx, v, ""); err != nil {
			return fmt.Errorf("recreate evicted pod %s: %w", podRef(v), err)
		}
	}

	return nil
}

// ---------------------------- Deletion helpers ----------------------------

func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	return wait.PollUntilContextTimeout(ctx, EvictionPollInterval, EvictionPollTimeout, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}

		for k := range remaining {
			got, err := pl.client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				delete(remaining, k) // gone
				continue
			}
			if err != nil {
				return false, nil
			}
			if string(got.UID) != k.uid {
				delete(remaining, k)
				continue
			}
		}

		if len(remaining) == 0 {
			return true, nil
		}
		klog.V(2).InfoS("Waiting for targeted pods to disappear",
			"remaining", len(remaining))
		return false, nil
	})
}

// Recreate a standalone pod without direct binding
func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.NodeName = "" // no direct binding
	newp.Spec.NodeSelector = map[string]string{}

	if _, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
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
	return pl.client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
}

// ---------------------------- Plan settle ----------------------------

func (pl *MyCrossNodePreemption) onPlanSettled() bool {
	sp, _ := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return false
	}
	klog.InfoS("plan settled; deactivating active plan")
	pl.clearActivePlan()
	pl.activateBlockedPods()
	return true
}

func (pl *MyCrossNodePreemption) activateBlockedPods() {
	if pl.blockedPods == nil {
		return
	}
	snap := pl.blockedPods.snapshot()
	if len(snap) == 0 {
		return
	}
	podLister := pl.handle.SharedInformerFactory().Core().V1().Pods().Lister()
	pods := make(map[string]*v1.Pod, len(snap))
	for _, bi := range snap {
		p, err := podLister.Pods(bi.ns).Get(bi.name)
		if err != nil {
			klog.ErrorS(err, "activateBlockedPods: lister get failed", "pod", bi.ns+"/"+bi.name)
			continue
		}
		pods[bi.ns+"/"+bi.name] = p
	}
	pl.handle.Activate(klog.Background(), pods)
	pl.blockedPods.clear()
}

func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
}

func (pl *MyCrossNodePreemption) getNodes() ([]*v1.Node, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.handle.SharedInformerFactory().Core().V1().Nodes().Lister().List(labels.Everything())
}

func (pl *MyCrossNodePreemption) getPods() ([]*v1.Pod, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.handle.SharedInformerFactory().Core().V1().Pods().Lister().List(labels.Everything())
}

func isNodeUsable(n *v1.Node) bool {
	if n == nil {
		return false
	}
	isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
	return !isCP &&
		!n.Spec.Unschedulable &&
		n.Status.Allocatable.Cpu().MilliValue() > 0 &&
		n.Status.Allocatable.Memory().Value() > 0
}

func (pl *MyCrossNodePreemption) runPythonOptimizerSingle(
	ctx context.Context,
	preemptor *v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {
	l := pl.handle.SnapshotSharedLister()
	if l == nil {
		return nil, fmt.Errorf("no snapshot lister")
	}
	nodeInfos, err := l.NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	type SolverInput struct {
		TimeoutMs      int64        `json:"timeout_ms"`
		IgnoreAffinity bool         `json:"ignore_affinity"`
		Preemptors     []SolverPod  `json:"preemptors"`
		Nodes          []SolverNode `json:"nodes"`
		Pods           []SolverPod  `json:"pods"`
	}
	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptors:     []SolverPod{toSolverPod(preemptor, "")},
	}

	usable := map[string]bool{}
	for _, ni := range nodeInfos {
		if ni == nil || ni.Node() == nil {
			continue
		}
		n := ni.Node()
		isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
			n.Labels["node-role.kubernetes.io/master"] != "" ||
			n.Name == "control-plane" || n.Name == "kind-control-plane"
		if isCP || n.Spec.Unschedulable || ni.Allocatable.MilliCPU <= 0 || ni.Allocatable.Memory <= 0 {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name: n.Name,
			CPU:  ni.Allocatable.MilliCPU,
			RAM:  ni.Allocatable.Memory,
		})
		usable[n.Name] = true
	}
	if len(in.Nodes) == 0 {
		return nil, fmt.Errorf("no usable nodes available (snapshot)")
	}

	for _, ni := range nodeInfos {
		if ni == nil || ni.Node() == nil {
			continue
		}
		for _, pi := range ni.Pods {
			where := pi.Pod.Spec.NodeName
			if where != "" && !usable[where] {
				continue
			}
			sp := toSolverPod(pi.Pod, where)
			if pi.Pod.Namespace == "kube-system" {
				sp.Protected = true
			}
			in.Pods = append(in.Pods, sp)
		}
	}
	in.Pods = append(in.Pods, toSolverPod(preemptor, ""))

	raw, _ := json.Marshal(in)
	klog.V(2).InfoS("Single-preemptor solver input", "nodes", len(in.Nodes), "pods", len(in.Pods))

	cmd := exec.CommandContext(ctx, "python3", PythonSolverPath)
	cmd.Stdin = bytes.NewReader(raw)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Run(); err != nil {
		klog.ErrorS(err, "python solver failed", "stderr", errBuf.String())
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if out.Status != "OK" {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}
