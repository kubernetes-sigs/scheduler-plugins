// plan_execution.go

package mycrossnodepreemption

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	// ======= Plan settings =======
	PlanExecutionTTL = 1 * time.Minute // how long a plan may run before being terminated

	ConfigMapNamespace = "kube-system"
	ConfigMapLabelKey  = "crossnode-plan"

	EvictionPollTimeout  = 20 * time.Second
	EvictionPollInterval = 1 * time.Second

	PythonSolverPath    = "/opt/solver/main.py"
	PythonSolverTimeout = 50 * time.Second
)

type StoredPlan struct {
	Completed        bool                      `json:"completed"`
	CompletedAt      *time.Time                `json:"completedAt,omitempty"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	PluginVersion    string                    `json:"pluginVersion"`
	PendingPod       string                    `json:"pendingPod"` // ns/name (kept for compatibility; any "lead" in cohort)
	PendingUID       string                    `json:"pendingUID"`
	TargetNode       string                    `json:"targetNode"` // may be empty in batch mode
	SolverOutput     *SolverOutput             `json:"solverOutput,omitempty"`
	Plan             PodAssignmentPlanLite     `json:"plan"`
	PlacementsByName map[string]string         `json:"placementsByName,omitempty"` // standalone pods -> node
	WkDesiredPerNode map[string]map[string]int `json:"wkDesiredPerNode,omitempty"` // "<ns>/<rs>" -> node -> count
}

type PodAssignmentPlanLite struct {
	TargetNode string         `json:"targetNode"`
	Movements  []MovementLite `json:"movements"`
	Evictions  []EvictionLite `json:"evictions"`
}
type MovementLite struct {
	Pod      PodRefLite `json:"pod"`
	FromNode string     `json:"fromNode"`
	ToNode   string     `json:"toNode"`
	CPUm     int64      `json:"cpu_m"`
	MemBytes int64      `json:"mem_bytes"`
}
type EvictionLite struct {
	Pod      PodRefLite `json:"pod"`
	FromNode string     `json:"fromNode"`
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

type SolverInput struct {
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Preemptor      *SolverPod   `json:"preemptor,omitempty"` // nil => batch mode
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
}

type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode,omitempty"`
	Placements    map[string]string `json:"placements"` // uid -> node
	Evictions     []SolverEviction  `json:"evictions"`
}

type WorkloadNodeCounters map[string]map[string]*atomic.Int32 // workloadKey -> node -> remaining

type PlanSlots struct {
	PlanID    string
	Remaining WorkloadNodeCounters
}

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
	pl.ActivePlan.Store(sp)
	pl.ActivePlanID.Store(id)

	// 1) desired per-node targets
	desired := map[string]map[string]int{}
	for rs, perNode := range sp.WkDesiredPerNode {
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
		addOut(ev.Pod.UID)
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
	counters := make(WorkloadNodeCounters, len(rem))
	for rs, byNode := range rem {
		inner := make(map[string]*atomic.Int32, len(byNode))
		for node, v := range byNode {
			ctr := new(atomic.Int32)
			ctr.Store(int32(v))
			inner[node] = ctr
		}
		counters[rs] = inner
	}
	ps := &PlanSlots{PlanID: id, Remaining: counters}
	pl.SlotsPtr.Store(ps)
}

func (pl *MyCrossNodePreemption) clearActivePlan() {
	pl.ActivePlan.Store((*StoredPlan)(nil))
	pl.ActivePlanID.Store("")
	pl.SlotsPtr.Store(nil)
}

func (pl *MyCrossNodePreemption) getActivePlan() (*StoredPlan, string) {
	v := pl.ActivePlan.Load()
	if v == nil {
		return nil, ""
	}
	return v.(*StoredPlan), pl.ActivePlanID.Load().(string)
}

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(ctx context.Context) ([]v1.ConfigMap, error) {
	lst, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).List(
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
		cm, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
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
			_, err = pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).
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
		if err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).
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
		preemptor, err := pl.Client.CoreV1().Pods(pns).Get(ctx, pname, metav1.GetOptions{})
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
		pod, err := pl.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
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
	for wkStr, perNode := range sp.WkDesiredPerNode {
		wk, ok := parseWorkloadKey(wkStr)
		if !ok {
			return false, nil
		}
		lblSel, err := selectorForWorkload(ctx, pl.Client, wk)
		if err != nil {
			return false, fmt.Errorf("selector for %s: %w", wk.String(), err)
		}
		selector, err := metav1.LabelSelectorAsSelector(&lblSel)
		if err != nil {
			return false, fmt.Errorf("build selector for %s: %w", wk.String(), err)
		}
		podList, err := pl.Client.CoreV1().Pods(wk.Namespace).List(ctx, metav1.ListOptions{
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
		lite.Evictions = append(lite.Evictions, EvictionLite{
			Pod:      PodRefLite{Namespace: v.Namespace, Name: v.Name, UID: string(v.UID)},
			FromNode: v.Spec.NodeName,
			CPUm:     getPodCPURequest(v),
			MemBytes: getPodMemoryRequest(v),
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
		leadIsPreemptor := out != nil && out.NominatedNode != ""
		if uid == string(pending.UID) && leadIsPreemptor {
			// In single-preemptor mode, the lead is bound by nominated node; don't allocate it via quotas.
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

// Cohort-capable runner
func (pl *MyCrossNodePreemption) runPythonOptimizerCohort(
	ctx context.Context,
	batched []*v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {
	// Live nodes
	nodes, err := pl.getNodes()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds() - 500, // leave some margin for returning results
		IgnoreAffinity: true,
		Preemptor:      nil, // batch mode
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
	seen := make(map[string]bool, len(allPods)+len(batched))
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
		seen[sp.UID] = true
	}

	// Append batched pending pods (no node) if not already present
	for _, p := range batched {
		sp := toSolverPod(p, "")
		if !seen[sp.UID] {
			in.Pods = append(in.Pods, sp)
			seen[sp.UID] = true
		}
	}

	raw, _ := json.Marshal(in)
	klog.V(2).InfoS("Batch solver input", "nodes", len(in.Nodes), "pods", len(in.Pods))

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
	okStatus := out.Status == "OPTIMAL" || out.Status == "FEASIBLE"
	if !okStatus {
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
	// In every-preemptor mode we expect NominatedNode != "".
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
		if from == "" {
			// Pending pod: not a "move" — let controllers/scheduler place it
			continue
		}
		if from == dest || dest == "" {
			continue
		}
		plan.PodMovements = append(plan.PodMovements, PodMovement{
			Pod: p, FromNode: from, ToNode: dest,
			CPURequest: getPodCPURequest(p), MemoryRequest: getPodMemoryRequest(p),
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
		Completed:        false,
		CompletedAt:      nil,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode, // may be empty in batch
		SolverOutput:     out,
		Plan:             litePlan,
		PlacementsByName: byName,
		WkDesiredPerNode: rsDesired,
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
	if _, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
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

	for _, mv := range plan.PodMovements {
		klog.V(2).InfoS("Pod movement planned",
			"pod", podRef(mv.Pod),
			"from", mv.FromNode,
			"to", mv.ToNode,
			"cpu(m)", mv.CPURequest,
			"mem(MiB)", bytesToMiB(mv.MemoryRequest),
		)
	}
	for _, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Eviction planned",
			"pod", podRef(v),
			"from", v.Spec.NodeName,
			"cpu(m)", getPodCPURequest(v),
			"mem(MiB)", bytesToMiB(getPodMemoryRequest(v)),
		)
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
			got, err := pl.Client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
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

func (pl *MyCrossNodePreemption) runPythonOptimizerSingle(
	ctx context.Context,
	preemptor *v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {
	l := pl.Handle.SnapshotSharedLister()
	if l == nil {
		return nil, fmt.Errorf("no snapshot lister")
	}
	nodeInfos, err := l.NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptor:      ptr(toSolverPod(preemptor, "")), // pending preemptor
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
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
			// Do NOT duplicate the preemptor
			if sp.UID == string(preemptor.UID) {
				continue
			}
			in.Pods = append(in.Pods, sp)
		}
	}

	raw, _ := json.Marshal(in)
	klog.V(2).InfoS("Single-preemptor solver input", "nodes", len(in.Nodes), "pods", len(in.Pods), "hasPreemptor", in.Preemptor != nil)

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
	okStatus := out.Status == "OPTIMAL" || out.Status == "FEASIBLE"
	if !okStatus {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}
