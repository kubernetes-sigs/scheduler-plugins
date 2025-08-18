package mycrossnodepreemption

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
	Version = "v1.0.0"
)

// path to your Python script inside the image
const pythonSolverPath = "/opt/solver/main.py"
const pythonSolverTimeout = 60 * time.Second

// ---------------------------- Plan export (ConfigMap) ----------------------------

// Minimal JSON persisted (no observed results).
type StoredPlan struct {
	SchemaVersion string    `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Plugin        string    `json:"plugin"`
	Version       string    `json:"pluginVersion"`

	PendingPod string `json:"pendingPod"` // ns/name
	PendingUID string `json:"pendingUID"`
	TargetNode string `json:"targetNode"`

	SolverOutput *solverOutput         `json:"solverOutput,omitempty"`
	Plan         PodAssignmentPlanLite `json:"plan"`

	// NEW: mirror of solverOutput.placements but keyed by pod *name*.
	PlacementsByName map[string]string `json:"placementsByName,omitempty"`
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
	exportNamespace  = "kube-system"
	exportCMLabelKey = "scheduler.x/crossnode-plan"
	exportCMLabelVal = "true"
)

// exportPlanToConfigMap writes plan.json to kube-system/<generated-name> and returns that name.
func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	plan *PodAssignmentPlan,
	out *solverOutput,
	pending *v1.Pod,
) (string, error) {

	// Convert to lite
	lite := PodAssignmentPlanLite{
		TargetNode: plan.TargetNode,
	}
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

	// build placements-by-name
	podsByUID := map[string]*v1.Pod{}
	if all, err := pl.handle.SnapshotSharedLister().NodeInfos().List(); err == nil {
		for _, ni := range all {
			for _, pi := range ni.Pods {
				podsByUID[string(pi.Pod.UID)] = pi.Pod
			}
		}
	}
	podsByUID[string(pending.UID)] = pending

	byName := make(map[string]string, len(out.Placements))
	for uid, node := range out.Placements {
		if p, ok := podsByUID[uid]; ok && p != nil {
			byName[p.Name] = node
		}
	}

	doc := &StoredPlan{
		SchemaVersion: "1",
		GeneratedAt:   time.Now().UTC(),
		Plugin:        Name,
		Version:       Version,

		PendingPod: fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID: string(pending.UID),
		TargetNode: plan.TargetNode,

		SolverOutput:     out,
		Plan:             lite,
		PlacementsByName: byName,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("crossnode-plan-%s-%d", pending.UID, time.Now().Unix())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: exportNamespace,
			Labels: map[string]string{
				exportCMLabelKey: exportCMLabelVal,
				"pendingPod":     pending.Name,
				"pendingNS":      pending.Namespace,
			},
		},
		Data: map[string]string{
			"plan.json": string(raw),
		},
	}

	if _, err := pl.client.CoreV1().ConfigMaps(exportNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return name, nil
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

// ---------------------------- PostFilter: call Python solver ----------------------------

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

	// bounded-time solver exec
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

	// Export ONLY the plan (no observed section)
	if cmName, err := pl.exportPlanToConfigMap(ctx, plan, out, pending); err != nil {
		klog.ErrorS(err, "Failed to export plan to ConfigMap (continuing)")
	} else {
		klog.V(2).InfoS("Exported plan", "configMap", fmt.Sprintf("%s/%s", exportNamespace, cmName))
	}

	// Execute your existing strategy (unchanged)
	if err := pl.executePlan(ctx, plan, pending); err != nil {
		klog.ErrorS(err, "plan execution failed")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.TargetNode},
	}, framework.NewStatus(framework.Success, "")
}

// ---------------------------- External solver I/O ----------------------------

type solverNode struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"` // milliCPU
	RAM    int64             `json:"ram"` // bytes
	Labels map[string]string `json:"labels,omitempty"`
}

type solverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"` // milliCPU request
	RAM       int64  `json:"ram"` // bytes request
	Priority  int32  `json:"priority"`
	Where     string `json:"where"` // node name or ""
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
	Placements    map[string]string `json:"placements"` // uid -> node
	Movements     map[string]string `json:"movements"`  // optional
	Evictions     []solverEviction  `json:"evictions"`
}

// ---------------------------- Bridge: Go -> Python ----------------------------

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
		Nodes:          []solverNode{},
		Pods:           []solverPod{},
	}

	// nodes first
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

	// existing pods only if they’re on a usable node
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

	// add pending
	in.Pods = append(in.Pods, toSolverPod(pending, ""))

	raw, err := json.Marshal(in)
	klog.V(5).InfoS("Solver input detail", "raw", string(raw))
	if err != nil {
		return nil, err
	}

	// call the python script (reads JSON from stdin, writes JSON to stdout)
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

// ---------------------------- Bridge: Python -> Go plan ----------------------------

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

func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	ctx context.Context,
	out *solverOutput,
	pending *v1.Pod,
) (*PodAssignmentPlan, error) {

	if out.NominatedNode == "" {
		return nil, fmt.Errorf("no nominated node for pending pod")
	}

	// lookup pods by UID
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
	// include pending pod
	podsByUID[string(pending.UID)] = pending

	plan := &PodAssignmentPlan{TargetNode: out.NominatedNode}

	// evictions
	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.VictimsToEvict = append(plan.VictimsToEvict, p)
		}
	}

	// movements for existing pods (infer from placements map)
	for uid, dest := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok {
			continue
		}
		if uid == string(pending.UID) {
			continue // pending will be scheduled on TargetNode
		}
		from := p.Spec.NodeName
		if from == dest || dest == "" {
			continue
		}
		mv := PodMovement{
			Pod:           p,
			FromNode:      from,
			ToNode:        dest,
			CPURequest:    getPodCPURequest(p),
			MemoryRequest: getPodMemoryRequest(p),
		}
		plan.PodMovements = append(plan.PodMovements, mv)
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

// helper: detect control-plane / master nodes
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

// ---------------------------- Utilities used by both files ----------------------------

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

func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func (pl *MyCrossNodePreemption) logPlan(plan *PodAssignmentPlan) {
	klog.InfoS("Execution plan",
		"targetNode", plan.TargetNode,
		"movements", len(plan.PodMovements),
		"evictions", len(plan.VictimsToEvict),
	)
	if len(plan.PodMovements) > 0 {
		for i, mv := range plan.PodMovements {
			klog.V(2).InfoS("Movement plan",
				"idx_move", i+1,
				"pod", podRef(mv.Pod),
				"from", mv.FromNode,
				"to", mv.ToNode,
				"cpu(m)", mv.CPURequest,
				"mem(bytes)", mv.MemoryRequest,
			)
		}
	}
	if len(plan.VictimsToEvict) > 0 {
		for i, v := range plan.VictimsToEvict {
			klog.V(2).InfoS("Eviction plan",
				"idx_evict", i+1,
				"pod", podRef(v),
				"node", v.Spec.NodeName,
				"cpu(m)", getPodCPURequest(v),
				"mem(bytes)", getPodMemoryRequest(v),
			)
		}
	}
}
