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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type MyCrossNodePreemption struct {
	handle        framework.Handle
	client        kubernetes.Interface
	mu            sync.Mutex
	processedPods map[string]int
}

// ---------------------------- Plugin wiring ----------------------------

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.0.2"
)

const pythonSolverPath = "/opt/solver/main.py"
const pythonSolverTimeout = 60 * time.Second

// ---------------------------- Plan export (ConfigMap) ----------------------------

type StoredPlan struct {
	SchemaVersion string    `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Plugin        string    `json:"plugin"`
	Version       string    `json:"pluginVersion"`

	PendingPod string `json:"pendingPod"` // ns/name
	PendingUID string `json:"pendingUID"`
	TargetNode string `json:"targetNode"`

	StopTheWorld bool `json:"stopTheWorld"`
	Completed    bool `json:"completed"`

	SolverOutput *solverOutput         `json:"solverOutput,omitempty"`
	Plan         PodAssignmentPlanLite `json:"plan"`

	// Standalone / specifically-named pods (incl. preemptor) -> node
	PlacementsByName map[string]string `json:"placementsByName,omitempty"`

	// NEW: desired RS replica distribution: "<ns>/<rs>" -> {"nodeA": count, ...}
	RSDesiredPerNode map[string]map[string]int `json:"rsDesiredPerNode,omitempty"`
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

const (
	exportNamespace         = "kube-system"
	exportCMLabelKey        = "scheduler.x/crossnode-plan"
	exportCMLabelVal        = "true"
	exportCMLabelActiveKey  = "scheduler.x/crossnode-plan-active"
	exportCMLabelActiveTrue = "true"
)

func rsKey(ns, rs string) string { return ns + "/" + rs }

// exportPlanToConfigMap builds PlacementsByName (for standalone + preemptor) and
// RSDesiredPerNode from solver placements (uid->node) by grouping RS-owned pods.
func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	plan *PodAssignmentPlan,
	out *solverOutput,
	pending *v1.Pod,
) (string, error) {

	// Lite view
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

	// Build uid -> pod map (snapshot)
	podsByUID := map[string]*v1.Pod{}
	if all, err := pl.handle.SnapshotSharedLister().NodeInfos().List(); err == nil {
		for _, ni := range all {
			for _, pi := range ni.Pods {
				podsByUID[string(pi.Pod.UID)] = pi.Pod
			}
		}
	}
	podsByUID[string(pending.UID)] = pending

	byName := make(map[string]string)
	rsDesired := map[string]map[string]int{} // rsKey -> node -> count

	for uid, node := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || p == nil {
			continue
		}
		if rsName, okRS := owningReplicaSet(p); okRS {
			k := rsKey(p.Namespace, rsName)
			if _, ok := rsDesired[k]; !ok {
				rsDesired[k] = map[string]int{}
			}
			rsDesired[k][node] = rsDesired[k][node] + 1
		} else {
			// Standalone/name-addressable pod
			byName[p.Name] = node
		}
	}

	doc := &StoredPlan{
		SchemaVersion:    "3",
		GeneratedAt:      time.Now().UTC(),
		Plugin:           Name,
		Version:          Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode,
		StopTheWorld:     true,
		Completed:        false,
		SolverOutput:     out,
		Plan:             lite,
		PlacementsByName: byName,
		RSDesiredPerNode: rsDesired,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	// Remove any previously active plans
	if err := pl.deactivateOldPlans(ctx); err != nil {
		klog.ErrorS(err, "Failed to deactivate old plans (continuing)")
	}

	name := fmt.Sprintf("crossnode-plan-%s-%d", pending.UID, time.Now().Unix())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: exportNamespace,
			Labels: map[string]string{
				exportCMLabelKey:       exportCMLabelVal,
				exportCMLabelActiveKey: exportCMLabelActiveTrue,
				"pendingPod":           pending.Name,
				"pendingNS":            pending.Namespace,
			},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.client.CoreV1().ConfigMaps(exportNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return name, nil
}

func (pl *MyCrossNodePreemption) deactivateOldPlans(ctx context.Context) error {
	list, err := pl.client.CoreV1().ConfigMaps(exportNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", exportCMLabelActiveKey, exportCMLabelActiveTrue),
	})
	if err != nil {
		return err
	}
	for i := range list.Items {
		_ = pl.client.CoreV1().ConfigMaps(exportNamespace).Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{})
	}
	return nil
}

func (pl *MyCrossNodePreemption) loadActivePlan(ctx context.Context) (*StoredPlan, string, error) {
	list, err := pl.client.CoreV1().ConfigMaps(exportNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", exportCMLabelActiveKey, exportCMLabelActiveTrue),
	})
	if err != nil {
		return nil, "", err
	}
	if len(list.Items) == 0 {
		return nil, "", nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.Time.After(list.Items[j].CreationTimestamp.Time)
	})
	cm := list.Items[0]
	raw := cm.Data["plan.json"]
	if raw == "" {
		return nil, "", fmt.Errorf("active plan missing plan.json")
	}
	var sp StoredPlan
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		return nil, "", err
	}
	return &sp, cm.Name, nil
}

func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = pl.client.CoreV1().ConfigMaps(exportNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
}

// ---------------------------- Initialization ----------------------------

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
	return &MyCrossNodePreemption{
		handle:        h,
		client:        client,
		processedPods: make(map[string]int),
	}, nil
}

// ---------------------------- Filter: enforce active plan with RS slots ----------------------------

func (pl *MyCrossNodePreemption) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
) *framework.Status {

	sp, cmName, err := pl.loadActivePlan(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to load active plan")
		return framework.NewStatus(framework.Error, err.Error())
	}
	if sp == nil || !sp.StopTheWorld || sp.Completed {
		return framework.NewStatus(framework.Success, "")
	}

	// If plan complete, lift immediately
	if pl.planLooksComplete(sp) {
		pl.markPlanCompleted(ctx, cmName)
		return framework.NewStatus(framework.Success, "")
	}

	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Unschedulable, "node info missing")
	}
	nodeName := nodeInfo.Node().Name

	// RS-owned pods: use RSDesiredPerNode slots (name-independent)
	if rsName, ok := owningReplicaSet(pod); ok {
		key := rsKey(pod.Namespace, rsName)
		targets, ok := sp.RSDesiredPerNode[key]
		if !ok {
			// During stop-the-world, block RS pods not in plan
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS not in active plan")
		}
		desired := targets[nodeName]
		if desired == 0 {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS has no slots on this node")
		}
		// Count current RS pods on this node using cached nodeInfo
		current := 0
		for _, pi := range nodeInfo.Pods {
			if pi.Pod.Namespace == pod.Namespace {
				if r, ok := owningReplicaSet(pi.Pod); ok && r == rsName {
					current++
				}
			}
		}
		if current >= desired {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS node quota reached")
		}
		// Slot available on this node -> allow
		return framework.NewStatus(framework.Success, "")
	}

	// Non-RS pods: only allow if explicitly planned by name for this node
	tgt, ok := sp.PlacementsByName[pod.Name]
	if !ok {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: pod not in active plan")
	}
	if tgt != nodeName {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: only planned node allowed")
	}
	return framework.NewStatus(framework.Success, "")
}

// planLooksComplete checks both:
// 1) Every name-addressed pod is bound to its target node.
// 2) For every RS target (key,node -> desired), the *current* node counts meet/exceed desired.
func (pl *MyCrossNodePreemption) planLooksComplete(sp *StoredPlan) bool {
	lister := pl.handle.SnapshotSharedLister()

	// (1) Standalone/name-addressed
	for name, node := range sp.PlacementsByName {
		ns := nsOf(sp.PendingPod) // conservative: assume same ns as pending; adjust if you store per-pod ns
		if strings.Contains(name, ".") {
			// If users run across namespaces, encode name as "ns/name" in PlacementsByName and parse here.
			parts := strings.SplitN(name, "/", 2)
			if len(parts) == 2 {
				ns, name = parts[0], parts[1]
			}
		}
		// Search pod across nodes quickly via lister
		found := false
		nodes, _ := lister.NodeInfos().List()
		for _, ni := range nodes {
			for _, pi := range ni.Pods {
				if pi.Pod.Namespace == ns && pi.Pod.Name == name {
					if ni.Node().Name == node {
						found = true
					}
				}
			}
		}
		if !found {
			return false
		}
	}

	// (2) RS targets
	for rsKeyStr, perNode := range sp.RSDesiredPerNode {
		ns, rs := splitNSName(rsKeyStr)
		// Count per node using lister
		nodeCounts := map[string]int{}
		nodes, _ := lister.NodeInfos().List()
		for _, ni := range nodes {
			c := 0
			for _, pi := range ni.Pods {
				if pi.Pod.Namespace == ns {
					if r, ok := owningReplicaSet(pi.Pod); ok && r == rs {
						c++
					}
				}
			}
			if c > 0 {
				nodeCounts[ni.Node().Name] = c
			}
		}
		// Check desired <= current
		for node, want := range perNode {
			if nodeCounts[node] < want {
				return false
			}
		}
	}
	return true
}

func nsOf(nsSlashName string) string {
	if i := strings.IndexByte(nsSlashName, '/'); i >= 0 {
		return nsSlashName[:i]
	}
	return "default"
}

func splitNSName(s string) (string, string) {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return "default", s
	}
	return s[:i], s[i+1:]
}

// ---------------------------- PostFilter (unchanged from prior edit) ----------------------------

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {

	klog.InfoS("PostFilter start", "pending pod", klog.KObj(pending),
		"cpu(m)", getPodCPURequest(pending),
		"mem(bytes)", getPodMemoryRequest(pending),
	)

	solveCtx, cancel := context.WithTimeout(ctx, pythonSolverTimeout+5*time.Second)
	defer cancel()

	startTime := time.Now()
	out, err := pl.runPythonOptimizer(solveCtx, pending, pythonSolverTimeout)
	if err != nil {
		klog.ErrorS(err, "optimizer error", "took", time.Since(startTime))
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	klog.InfoS("Solver executed successfully", "status", out.Status, "took", time.Since(startTime))

	plan, err := pl.translatePlanFromSolver(ctx, out, pending)
	if err != nil {
		klog.ErrorS(err, "build plan error")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	if plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		return nil, framework.NewStatus(framework.Unschedulable, "no actionable plan")
	}

	pl.logPlan(plan)

	if cmName, err := pl.exportPlanToConfigMap(ctx, plan, out, pending); err != nil {
		klog.ErrorS(err, "Failed to export plan to ConfigMap (continuing)")
	} else {
		klog.V(2).InfoS("Exported active plan", "configMap", fmt.Sprintf("%s/%s", exportNamespace, cmName))
	}

	if err := pl.executePlan(ctx, plan, pending); err != nil {
		klog.ErrorS(err, "plan execution failed")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.TargetNode},
	}, framework.NewStatus(framework.Success, "")
}

// ---------------------------- Solver I/O, utilities (unchanged) ----------------------------

type solverNode struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"`
	RAM    int64             `json:"ram"`
	Labels map[string]string `json:"labels,omitempty"`
}
type solverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	RAM       int64  `json:"ram"`
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`
	Protected bool   `json:"protected,omitempty"`
}
type solverInput struct {
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Preemptor      solverPod    `json:"preemptor"`
	Nodes          []solverNode `json:"nodes"`
	Pods           []solverPod  `json:"pods"`
}
type solverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type solverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode"`
	Placements    map[string]string `json:"placements"`
	Movements     map[string]string `json:"movements"`
	Evictions     []solverEviction  `json:"evictions"`
}

func (pl *MyCrossNodePreemption) runPythonOptimizer(
	ctx context.Context,
	pending *v1.Pod,
	timeout time.Duration,
) (*solverOutput, error) {
	nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	in := solverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptor:      toSolverPod(pending, ""),
	}
	usable := map[string]bool{}
	for _, ni := range nodes {
		if !isNodeUsableFor(pending, ni) {
			continue
		}
		in.Nodes = append(in.Nodes, solverNode{
			Name: ni.Node().Name,
			CPU:  ni.Allocatable.MilliCPU,
			RAM:  ni.Allocatable.Memory,
		})
		usable[ni.Node().Name] = true
	}
	for _, ni := range nodes {
		if !isNodeUsableFor(pending, ni) {
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
	in.Pods = append(in.Pods, toSolverPod(pending, ""))

	raw, err := json.Marshal(in)
	klog.V(5).InfoS("Solver input detail", "raw", string(raw))
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "python3", pythonSolverPath)
	cmd.Stdin = bytes.NewReader(raw)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Run(); err != nil {
		klog.ErrorS(err, "python solver failed", "stderr", errBuf.String())
		return nil, fmt.Errorf("solver run: %w", err)
	}
	var out solverOutput
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if out.Status != "OK" {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}

func toSolverPod(p *v1.Pod, where string) solverPod {
	return solverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU:       getPodCPURequest(p),
		RAM:       getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
	}
}

// Plan & helpers (unchanged)
type PodAssignmentPlan struct {
	TargetNode     string
	PodMovements   []PodMovement
	VictimsToEvict []*v1.Pod
}
type PodMovement struct {
	Pod           *v1.Pod
	FromNode      string
	ToNode        string
	CPURequest    int64
	MemoryRequest int64
}

func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	ctx context.Context,
	out *solverOutput,
	pending *v1.Pod,
) (*PodAssignmentPlan, error) {
	if out.NominatedNode == "" {
		return nil, fmt.Errorf("no nominated node for pending pod")
	}
	all, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, err
	}
	podsByUID := map[string]*v1.Pod{}
	for _, ni := range all {
		for _, pi := range ni.Pods {
			podsByUID[string(pi.Pod.UID)] = pi.Pod
		}
	}
	podsByUID[string(pending.UID)] = pending
	plan := &PodAssignmentPlan{TargetNode: out.NominatedNode}
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

func toleratesNoScheduleTaints(pod *v1.Pod, taints []v1.Taint) bool {
	for _, t := range taints {
		if t.Effect != v1.TaintEffectNoSchedule {
			continue
		}
		tolerated := false
		for _, tol := range pod.Spec.Tolerations {
			if tol.ToleratesTaint(&t) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func isControlPlane(n *v1.Node) bool {
	if n == nil {
		return false
	}
	labels := n.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	if n.Name == "control-plane" || n.Name == "kind-control-plane" {
		return true
	}
	return false
}

func isNodeUsableFor(pod *v1.Pod, ni *framework.NodeInfo) bool {
	n := ni.Node()
	if n == nil {
		return false
	}
	if isControlPlane(n) {
		return false
	}
	if n.Spec.Unschedulable {
		return false
	}
	if !toleratesNoScheduleTaints(pod, n.Spec.Taints) {
		return false
	}
	if ni.Allocatable.MilliCPU <= 0 || ni.Allocatable.Memory <= 0 {
		return false
	}
	return true
}

// Utilities
func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceCPU]; !req.IsZero() {
			total += req.MilliValue()
		}
	}
	return total
}
func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceMemory]; !req.IsZero() {
			total += req.Value()
		}
	}
	return total
}
func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}
func podRef(p *v1.Pod) string { return fmt.Sprintf("%s/%s", p.Namespace, p.Name) }

func (pl *MyCrossNodePreemption) logPlan(plan *PodAssignmentPlan) {
	klog.InfoS("Execution plan",
		"targetNode", plan.TargetNode,
		"movements", len(plan.PodMovements),
		"evictions", len(plan.VictimsToEvict),
	)
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("Movement plan",
			"idx_move", i+1, "pod", podRef(mv.Pod),
			"from", mv.FromNode, "to", mv.ToNode,
			"cpu(m)", mv.CPURequest, "mem(bytes)", mv.MemoryRequest,
		)
	}
	for i, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Eviction plan",
			"idx_evict", i+1, "pod", podRef(v),
			"node", v.Spec.NodeName,
			"cpu(m)", getPodCPURequest(v), "mem(bytes)", getPodMemoryRequest(v),
		)
	}
}
