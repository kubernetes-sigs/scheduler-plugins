package mycrossnodepreemption

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type MyCrossNodePreemption struct {
	handle        framework.Handle
	client        kubernetes.Interface
	args          *Config
	mu            sync.Mutex
	processedPods map[string]int
}

// ---------------------------- Plugin wiring ----------------------------

const (
	Name               = "MyCrossNodePreemption"
	Version            = "v1.15.0"
	maxPostFilterTries = 3
)

type Config struct {
	MaxMovesPerPod int `json:"maxMovesPerPod,omitempty"`
}

// path to your Python script inside the image
const pythonSolverPath = "/opt/solver/main.py"

// ---------------------------- Initialization ----------------------------

func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	cfg := &Config{MaxMovesPerPod: 5}
	if obj != nil {
		klog.V(2).InfoS("Plugin configuration", "config", obj)
	}

	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}

	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "moveBudget", cfg.MaxMovesPerPod)
	return &MyCrossNodePreemption{
		handle:        h,
		client:        client,
		args:          cfg,
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

	klog.InfoS("PostFilter start (python CP-SAT)", "pod", klog.KObj(pending))

	// give the external solver a bounded time
	solveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := pl.runPythonOptimizer(solveCtx, pending, 4*time.Second)
	if err != nil {
		klog.ErrorS(err, "optimizer error")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}

	plan, err := pl.planFromSolver(ctx, out, pending)
	if err != nil {
		klog.ErrorS(err, "build plan error")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	if plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		return nil, framework.NewStatus(framework.Unschedulable, "no actionable plan")
	}

	// Execute plan: moves and evictions
	if err := pl.executePlan(ctx, plan); err != nil {
		klog.ErrorS(err, "plan execution failed")
		return nil, framework.NewStatus(framework.Error, err.Error())
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
	CPU       int64  `json:"cpu"`     // milliCPU request
	RAM       int64  `json:"ram"`     // bytes request
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`   // node name or ""
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
		if !isNodeUsableFor(pending, ni) { continue }
		in.Nodes = append(in.Nodes, solverNode{
			Name: ni.Node().Name,
			CPU:  ni.Allocatable.MilliCPU,
			RAM:  ni.Allocatable.Memory,
		})
		usable[ni.Node().Name] = true
	}

	// existing pods only if they’re on a usable node
	for _, ni := range nodes {
		if !isNodeUsableFor(pending, ni) { continue }
		for _, pi := range ni.Pods {
			where := pi.Pod.Spec.NodeName
			if where != "" && !usable[where] { continue }
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

type BinPackingPlan struct {
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

func (pl *MyCrossNodePreemption) planFromSolver(
	ctx context.Context,
	out *solverOutput,
	pending *v1.Pod,
) (*BinPackingPlan, error) {

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

	plan := &BinPackingPlan{TargetNode: out.NominatedNode}

	// evictions
	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.VictimsToEvict = append(plan.VictimsToEvict, p)
		}
	}

	// movements for existing pods
	for uid, dest := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok {
			continue
		}
		if uid == string(pending.UID) {
			continue // pending will be scheduled by kube-scheduler on TargetNode
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
func isNodeUsableFor(pod *v1.Pod, ni *framework.NodeInfo) bool {
	n := ni.Node()
	if n == nil {
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

func isControlPlaneNode(name string) bool {
	return strings.Contains(name, "control-plane") || strings.Contains(name, "master")
}
