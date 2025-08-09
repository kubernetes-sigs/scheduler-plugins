package mycrossnodepreemptionbinpacking

import (
	"context"
	"sort"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type MyCrossNodePreemptionBinpacking struct {
	handle        framework.Handle
	client        kubernetes.Interface
	args          *Config
	mu            sync.Mutex
	processedPods map[string]int
}

// ---------------------------- Plugin wiring ----------------------------

const (
	Name               = "MyCrossNodePreemptionBinpacking"
	Version            = "v1.14.0"
	maxPostFilterTries = 3
)

type Config struct {
	MaxMovesPerPod int `json:"maxMovesPerPod,omitempty"`
}

func (pl *MyCrossNodePreemptionBinpacking) Name() string { return Name }

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
	return &MyCrossNodePreemptionBinpacking{
		handle:        h,
		client:        client,
		args:          cfg,
		processedPods: make(map[string]int),
	}, nil
}

// ---------------------------- PostFilter ----------------------------

func (pl *MyCrossNodePreemptionBinpacking) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, _ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
    klog.InfoS("PostFilter start", "pod", klog.KObj(pod))

    nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List() // get all nodes incl. resource usage
    if err != nil || len(nodes) == 0 {
        if err != nil { klog.ErrorS(err, "list nodes") }
        return nil, framework.NewStatus(framework.Unschedulable, "no nodes")
    }

    candidates := pl.buildCandidates(nodes, pod) // candidates for possible preemption
    solution := pl.findBestPlan(candidates, pod) // find the best bin-packing solution

	// If no solution found the PostFilter will return unschedulable status
	if solution == nil { return nil, framework.NewStatus(framework.Unschedulable, "no preemption candidates found") }
    
	// Execute the bin-packing solution
	if err := pl.executeBinPackingSolution(ctx, solution); err != nil {
        klog.ErrorS(err, "plan execution failed")
        return nil, framework.NewStatus(framework.Error, err.Error())
    }
	
	// Update the nominated node (i.e. target node where the new pod will be scheduled)
    return &framework.PostFilterResult{
        NominatingInfo: &framework.NominatingInfo{NominatedNodeName: solution.TargetNode},
    }, framework.NewStatus(framework.Success, "")
}

// ---------------------------- Modeling ----------------------------

type Candidate struct {
	NodeName  string
	State     *NodeResourceState
	Victims   []*v1.Pod // lower-priority pods on node
}

type NodeResourceState struct {
	Name              string
	AvailableCPU      int64
	AvailableMemory   int64
	AllocatableCPU    int64
	AllocatableMemory int64
	Pods              []*v1.Pod
}

type BinPackingSolution struct {
	TargetNode     string
	PodMovements   []PodMovement // includes helper moves
	VictimsToEvict []*v1.Pod     // if needed after moves
}

type PodMovement struct {
	Pod        *v1.Pod
	FromNode   string
	ToNode     string
	CPURequest int64
}

// buildCandidates creates a list of candidate nodes for preemption based on resource availability
func (pl *MyCrossNodePreemptionBinpacking) buildCandidates(
	nodes []*framework.NodeInfo, pod *v1.Pod,
) []*Candidate {
	var out []*Candidate
	for _, nodeInfo := range nodes {
		if isControlPlaneNode(nodeInfo.Node().Name) {
			continue
		}
		availCPU := nodeInfo.Allocatable.MilliCPU - nodeInfo.Requested.MilliCPU
		if availCPU < 0 {
			availCPU = 0
		}
		availMem := nodeInfo.Allocatable.Memory - nodeInfo.Requested.Memory
		if availMem < 0 {
			availMem = 0
		}
		st := &NodeResourceState{
			Name:              nodeInfo.Node().Name,
			AvailableCPU:      availCPU,
			AvailableMemory:   availMem,
			AllocatableCPU:    nodeInfo.Allocatable.MilliCPU,
			AllocatableMemory: nodeInfo.Allocatable.Memory,
			Pods:              []*v1.Pod{},
		}
		var lows []*v1.Pod
		for _, pi := range nodeInfo.Pods {
			st.Pods = append(st.Pods, pi.Pod)
			if pi.Pod.DeletionTimestamp == nil && getPodPriority(pi.Pod) < getPodPriority(pod) {
				lows = append(lows, pi.Pod)
			}
		}
		out = append(out, &Candidate{NodeName: nodeInfo.Node().Name, State: st, Victims: lows})
	}
	return out
}

// ---------------------------- Planner ----------------------------
// findBestPlan identifies the best bin-packing plan from the available candidates
func (pl *MyCrossNodePreemptionBinpacking) findBestPlan(cands []*Candidate, pending *v1.Pod) *BinPackingSolution {
	all := map[string]*NodeResourceState{}
	for _, c := range cands {
		all[c.NodeName] = c.State
	}

	var best *BinPackingSolution
	bestEvict := int(1 << 30)
	bestMoves := int(1 << 30)

	for _, c := range cands {
		if isControlPlaneNode(c.NodeName) { continue }
		cpuDef := getPodCPURequest(pending) - c.State.AvailableCPU
		memDef := getPodMemoryRequest(pending) - c.State.AvailableMemory
		if cpuDef < 0 { cpuDef = 0 }
		if memDef < 0 { memDef = 0 }

		plan := pl.findPlanForTarget(pending, c.NodeName, cpuDef, memDef, all)
		if plan == nil { continue }
		ev, mv := len(plan.VictimsToEvict), len(plan.PodMovements)
		if ev < bestEvict || (ev == bestEvict && mv < bestMoves) {
			best, bestEvict, bestMoves = plan, ev, mv
		}
	}
	return best
}

// findPlanForTarget attempts to find a bin-packing plan for a specific target node
func (pl *MyCrossNodePreemptionBinpacking) findPlanForTarget(
	pending *v1.Pod, target string, cpuDef, memDef int64,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {
	state := nodeStates[target]
	if state == nil {
		return nil
	}
	movable := pl.getMovablePods(state.Pods, pending)

	// simple greedy: largest first tends to minimize #moves/evictions
	sort.Slice(movable, func(i, j int) bool {
		return getPodCPURequest(movable[i]) > getPodCPURequest(movable[j])
	})

	return pl.greedyMoveThenEvict(pending, target, cpuDef, memDef, movable, nodeStates)
}

// Greedy: try moves first (with ONE helper layer to free a destination), then evict the
// smallest remaining set if still short. Tie-breaks are handled by caller.
func (pl *MyCrossNodePreemptionBinpacking) greedyMoveThenEvict(
	pending *v1.Pod, target string, cpuDef, memDef int64,
	movable []*v1.Pod, global map[string]*NodeResourceState,
) *BinPackingSolution {

	sim := pl.copyNodeStates(global)
	solution := &BinPackingSolution{TargetNode: target}

	var freedCPU, freedMem int64
	moveBudget := pl.args.MaxMovesPerPod
	var couldntMove []*v1.Pod

	for _, pod := range movable {
		if pod.Spec.NodeName != target {
			continue
		}
		if moveBudget <= 0 {
			couldntMove = append(couldntMove, pod)
			continue
		}

		dest := pl.bestDestExcluding(pod, sim, map[string]bool{target: true})
		helpers := []PodMovement{}
		if dest == "" && moveBudget > 1 {
			dest, helpers = pl.freeDestWithOneHelperLayer(pod, pending, target, sim, moveBudget-1)
		}
		if dest == "" {
			couldntMove = append(couldntMove, pod)
			continue
		}

		for _, mv := range helpers {
			applyMove(sim, mv)
			solution.PodMovements = append(solution.PodMovements, mv)
			moveBudget--
		}
		mv := PodMovement{Pod: pod, FromNode: target, ToNode: dest, CPURequest: getPodCPURequest(pod)}
		applyMove(sim, mv)
		solution.PodMovements = append(solution.PodMovements, mv)
		moveBudget--

		freedCPU += getPodCPURequest(pod)
		freedMem += getPodMemoryRequest(pod)
		if freedCPU >= cpuDef && freedMem >= memDef { break }
	}

	if freedCPU < cpuDef || freedMem < memDef {
		sort.Slice(couldntMove, func(i, j int) bool {
			return getPodCPURequest(couldntMove[i]) < getPodCPURequest(couldntMove[j])
		})
		for _, p := range couldntMove {
			if freedCPU >= cpuDef && freedMem >= memDef { break }
			solution.VictimsToEvict = append(solution.VictimsToEvict, p)
			freedCPU += getPodCPURequest(p)
			freedMem += getPodMemoryRequest(p)
		}
		if freedCPU < cpuDef || freedMem < memDef { return nil }
	}
	return solution
}

// One helper layer: choose a destination and (if needed) free it by moving a single chain of pods off it.
func (pl *MyCrossNodePreemptionBinpacking) freeDestWithOneHelperLayer(
	relocating, pending *v1.Pod, target string,
	sim map[string]*NodeResourceState, moveBudget int,
) (string, []PodMovement) {
	needCPU := getPodCPURequest(relocating)
	needMem := getPodMemoryRequest(relocating)

	type destCand struct { name string; cpuGap, memGap int64 }
	var cands []destCand
	for name, st := range sim {
		if name == target || isControlPlaneNode(name) || !isNodeSchedulable(st) { continue }
		if st.AvailableCPU >= needCPU && st.AvailableMemory >= needMem {
			return name, nil
		}
		cpuGap := needCPU - st.AvailableCPU
		memGap := needMem - st.AvailableMemory
		if cpuGap > 0 || memGap > 0 {
			cands = append(cands, destCand{name, cpuGap, memGap}) // use the TYPE, not the slice name
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].cpuGap < cands[j].cpuGap })

	for _, dc := range cands {
		if moveBudget <= 0 { break }
		loc := pl.copyNodeStates(sim)
		var plan []PodMovement

		var destPods []*v1.Pod
		for _, p := range loc[dc.name].Pods {
			if p.Namespace == "kube-system" { continue }
			if getPodPriority(p) >= getPodPriority(pending) { continue }
			if p.Spec.NodeName == dc.name { destPods = append(destPods, p) }
		}
		sort.Slice(destPods, func(i, j int) bool { return getPodCPURequest(destPods[i]) > getPodCPURequest(destPods[j]) })

		for _, dp := range destPods {
			if moveBudget <= 0 { break }
			excl := map[string]bool{dc.name: true, target: true}
			tgt := pl.bestDestExcluding(dp, loc, excl)
			if tgt == "" { continue }
			mv := PodMovement{Pod: dp, FromNode: dc.name, ToNode: tgt, CPURequest: getPodCPURequest(dp)}
			applyMove(loc, mv)
			plan = append(plan, mv)
			moveBudget--

			if loc[dc.name].AvailableCPU >= needCPU && loc[dc.name].AvailableMemory >= needMem {
				return dc.name, plan
			}
		}
	}
	return "", nil
}

// ---------------------------- Node selection helpers ----------------------------
// bestDestExcluding finds the best destination node for a pod, excluding certain nodes (e.g., the current node)
func (pl *MyCrossNodePreemptionBinpacking) bestDestExcluding(
	pod *v1.Pod,
	nodeStates map[string]*NodeResourceState,
	exclude map[string]bool,
) string {
	pCPU := getPodCPURequest(pod)
	pMem := getPodMemoryRequest(pod)
	var best string
	bestUtil := 2.0 // min utilization after placing the pod
	for name, st := range nodeStates {
		if exclude[name] || isControlPlaneNode(name) || !isNodeSchedulable(st) {
			continue
		}
		if st.AvailableCPU >= pCPU && st.AvailableMemory >= pMem {
			newUsed := (st.AllocatableCPU - st.AvailableCPU) + pCPU
			util := float64(newUsed) / float64(st.AllocatableCPU)
			if util < bestUtil {
				best, bestUtil = name, util
			}
		}
	}
	return best
}

// ---------------------------- Small utilities ----------------------------

// applyMove simulates the movement of a pod from one node to another to test the impact on resource allocation
func applyMove(sim map[string]*NodeResourceState, mv PodMovement) {
	cpu := mv.CPURequest
	mem := getPodMemoryRequest(mv.Pod)

	from := sim[mv.FromNode]
	to := sim[mv.ToNode]

	from.AvailableCPU += cpu
	from.AvailableMemory += mem
	to.AvailableCPU -= cpu
	to.AvailableMemory -= mem

	// update pod lists
	var kept []*v1.Pod
	for _, x := range from.Pods {
		if x.UID != mv.Pod.UID {
			kept = append(kept, x)
		}
	}
	from.Pods = kept
	to.Pods = append(to.Pods, mv.Pod)
}

// getMovablePods returns a list of pods that can be moved to accommodate the target pod
func (pl *MyCrossNodePreemptionBinpacking) getMovablePods(pods []*v1.Pod, target *v1.Pod) []*v1.Pod {
	var out []*v1.Pod
	for _, p := range pods {
		if p.Namespace == target.Namespace && p.Name == target.Name {
			continue
		}
		if p.Namespace == "kube-system" {
			continue
		}
		if getPodPriority(p) < getPodPriority(target) {
			out = append(out, p)
		}
	}
	return out
}

// copyNodeStates creates a deep copy of the node states to be used for simulation
func (pl *MyCrossNodePreemptionBinpacking) copyNodeStates(m map[string]*NodeResourceState) map[string]*NodeResourceState {
	cp := make(map[string]*NodeResourceState, len(m))
	for k, v := range m {
		pods := make([]*v1.Pod, len(v.Pods))
		copy(pods, v.Pods)
		cp[k] = &NodeResourceState{
			Name:              v.Name,
			AvailableCPU:      v.AvailableCPU,
			AvailableMemory:   v.AvailableMemory,
			AllocatableCPU:    v.AllocatableCPU,
			AllocatableMemory: v.AllocatableMemory,
			Pods:              pods,
		}
	}
	return cp
}

// getPodCPURequest returns the total CPU request of a pod
func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceCPU]; !req.IsZero() {
			total += req.MilliValue()
		}
	}
	return total
}

// getPodMemoryRequest returns the total memory request of a pod
func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceMemory]; !req.IsZero() {
			total += req.Value()
		}
	}
	return total
}

// getPodPriority returns the priority of a pod
func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}

// isControlPlaneNode checks if a node is a control plane node
func isControlPlaneNode(name string) bool {
	return strings.Contains(name, "control-plane") || strings.Contains(name, "master")
}

// isNodeSchedulable checks if a node is schedulable by checking its resource availability
func isNodeSchedulable(st *NodeResourceState) bool {
	return st.AllocatableCPU > 0 && st.AllocatableMemory > 0
}