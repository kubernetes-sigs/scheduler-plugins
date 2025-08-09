package mycrossnodepreemptionbinpacking

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// ---------------------------- Plugin wiring ----------------------------

type MyCrossNodePreemptionBinpacking struct {
	handle        framework.Handle
	client        kubernetes.Interface
	args          *Config
	mu            sync.Mutex
	processedPods map[string]int // tries per pod UID
}

const (
	Name               = "MyCrossNodePreemptionBinpacking"
	Version            = "v1.12.0-slim"
	maxPostFilterTries = 3
)

type Config struct {
	MaxMovesPerPod int           `json:"maxMovesPerPod,omitempty"` // total move budget (incl. helpers)
}

func (pl *MyCrossNodePreemptionBinpacking) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	cfg := &Config{ MaxMovesPerPod: 5 }
	if obj != nil {
		klog.V(2).InfoS("Plugin configuration", "config", obj)
	}

	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
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

func (pl *MyCrossNodePreemptionBinpacking) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {
	defer klog.V(2).InfoS("PostFilter completed", "pod", klog.KObj(pod))

	klog.InfoS("PostFilter start", "pod", klog.KObj(pod), "priorityClass", pod.Spec.PriorityClassName)

	// bound retries so we don't loop forever on a tough case
	key := string(pod.UID)
	pl.mu.Lock()
	if pl.processedPods[key] >= maxPostFilterTries {
		pl.mu.Unlock()
		return nil, framework.NewStatus(framework.Unschedulable, "already processed")
	}
	pl.processedPods[key]++
	pl.mu.Unlock()

	nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "failed to list nodes")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}
	if len(nodes) == 0 {
		return nil, framework.NewStatus(framework.Unschedulable, "no nodes")
	}

	cands := pl.buildCandidates(nodes, pod)
	sol := pl.findBestPlan(cands, pod)
	if sol == nil {
		klog.V(2).InfoS("No plan found", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.Unschedulable, "no preemption candidates found")
	}

	klog.InfoS("Plan chosen",
		"targetNode", sol.TargetNode,
		"movements", len(sol.PodMovements),
		"evictions", len(sol.VictimsToEvict))

	if err := pl.executeBinPackingSolution(ctx, sol); err != nil {
		klog.ErrorS(err, "plan execution failed")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

	// success → clear retry counter
	pl.mu.Lock()
	delete(pl.processedPods, key)
	pl.mu.Unlock()

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: sol.TargetNode},
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

func (pl *MyCrossNodePreemptionBinpacking) buildCandidates(
	nodes []*framework.NodeInfo, pod *v1.Pod,
) []*Candidate {
	var out []*Candidate
	for _, ni := range nodes {
		if isControlPlaneNode(ni.Node().Name) {
			continue
		}
		availCPU := ni.Allocatable.MilliCPU - ni.Requested.MilliCPU
		if availCPU < 0 {
			availCPU = 0
		}
		availMem := ni.Allocatable.Memory - ni.Requested.Memory
		if availMem < 0 {
			availMem = 0
		}
		st := &NodeResourceState{
			Name:              ni.Node().Name,
			AvailableCPU:      availCPU,
			AvailableMemory:   availMem,
			AllocatableCPU:    ni.Allocatable.MilliCPU,
			AllocatableMemory: ni.Allocatable.Memory,
			Pods:              []*v1.Pod{},
		}
		var lows []*v1.Pod
		for _, pi := range ni.Pods {
			st.Pods = append(st.Pods, pi.Pod)
			if pi.Pod.DeletionTimestamp == nil && getPodPriority(pi.Pod) < getPodPriority(pod) {
				lows = append(lows, pi.Pod)
			}
		}
		out = append(out, &Candidate{NodeName: ni.Node().Name, State: st, Victims: lows})
	}
	return out
}

// ---------------------------- Planner (slim) ----------------------------

func (pl *MyCrossNodePreemptionBinpacking) findBestPlan(cands []*Candidate, pending *v1.Pod) *BinPackingSolution {
	all := map[string]*NodeResourceState{}
	for _, c := range cands {
		all[c.NodeName] = c.State
	}

	var best *BinPackingSolution
	bestEvict := int(1<<30)
	bestMoves := int(1<<30)

	for _, c := range cands {
		if isControlPlaneNode(c.NodeName) {
			continue
		}
		cpuDef := getPodCPURequest(pending) - c.State.AvailableCPU
		memDef := getPodMemoryRequest(pending) - c.State.AvailableMemory
		if cpuDef < 0 {
			cpuDef = 0
		}
		if memDef < 0 {
			memDef = 0
	}
		plan := pl.findPlanForTarget(pending, c.NodeName, cpuDef, memDef, all)
		if plan == nil {
			continue
		}
		ev := len(plan.VictimsToEvict)
		mv := len(plan.PodMovements)
		if ev < bestEvict || (ev == bestEvict && mv < bestMoves) {
			best, bestEvict, bestMoves = plan, ev, mv
		}
	}
	return best
}

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
	pending *v1.Pod,
	target string,
	cpuDef, memDef int64,
	movable []*v1.Pod,
	global map[string]*NodeResourceState,
) *BinPackingSolution {

	sim := pl.copyNodeStates(global)
	sol := &BinPackingSolution{TargetNode: target}

	var freedCPU, freedMem int64
	moveBudget := pl.args.MaxMovesPerPod
	var couldntMove []*v1.Pod

	for _, p := range movable {
		if p.Spec.NodeName != target {
			continue
		}
		if moveBudget <= 0 {
			couldntMove = append(couldntMove, p)
			continue
		}

		// try direct best destination
		dest := pl.bestDestExcluding(p, sim, map[string]bool{target: true})
		helpers := []PodMovement{}

		// if no direct fit, try one helper layer to free some node
		if dest == "" && moveBudget > 1 {
			dest, helpers = pl.freeDestWithOneHelperLayer(p, pending, target, sim, moveBudget-1)
		}
		if dest == "" {
			couldntMove = append(couldntMove, p)
			continue
		}

		// apply helpers then the main move
		for _, mv := range helpers {
			applyMove(sim, mv)
			sol.PodMovements = append(sol.PodMovements, mv)
			moveBudget--
		}
		mv := PodMovement{Pod: p, FromNode: target, ToNode: dest, CPURequest: getPodCPURequest(p)}
		applyMove(sim, mv)
		sol.PodMovements = append(sol.PodMovements, mv)
		moveBudget--

		freedCPU += getPodCPURequest(p)
		freedMem += getPodMemoryRequest(p)
		if freedCPU >= cpuDef && freedMem >= memDef {
			break
		}
	}

	// If still short → evict smallest set of the remaining movable pods.
	if freedCPU < cpuDef || freedMem < memDef {
		sort.Slice(couldntMove, func(i, j int) bool {
			return getPodCPURequest(couldntMove[i]) < getPodCPURequest(couldntMove[j])
		})
		for _, p := range couldntMove {
			if freedCPU >= cpuDef && freedMem >= memDef {
				break
			}
			sol.VictimsToEvict = append(sol.VictimsToEvict, p)
			freedCPU += getPodCPURequest(p)
			freedMem += getPodMemoryRequest(p)
		}
		if freedCPU < cpuDef || freedMem < memDef {
			return nil // even after evictions we couldn't make room
		}
	}
	return sol
}

// One helper layer: choose a destination and (if needed) free it by moving a single chain of pods off it.
func (pl *MyCrossNodePreemptionBinpacking) freeDestWithOneHelperLayer(
	relocating *v1.Pod,
	pending *v1.Pod,
	target string,
	sim map[string]*NodeResourceState,
	moveBudget int, // helpers budget only (caller has already reserved 1 for the main move)
) (string, []PodMovement) {
	needCPU := getPodCPURequest(relocating)
	needMem := getPodMemoryRequest(relocating)

	// Rank candidate destinations by how close they are (CPU gap)
	type cand struct {
		name         string
		cpuGap, memGap int64
	}
	var cands []cand
	for name, st := range sim {
		if name == target || isControlPlaneNode(name) || !isNodeSchedulable(st) {
			continue
		}
		if st.AvailableCPU >= needCPU && st.AvailableMemory >= needMem {
			// already fits (we shouldn't be here, but handle gracefully)
			return name, nil
		}
		cpuGap := needCPU - st.AvailableCPU
		memGap := needMem - st.AvailableMemory
		if cpuGap > 0 || memGap > 0 {
			cands = append(cands, cand{name, cpuGap, memGap})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].cpuGap < cands[j].cpuGap })

	for _, cd := range cands {
		if moveBudget <= 0 {
			break
		}
		loc := pl.copyNodeStates(sim)
		var plan []PodMovement

		// Lower-priority pods on the would-be destination
		var destPods []*v1.Pod
		for _, p := range loc[cd.name].Pods {
			if p.Namespace == "kube-system" || controlledByDaemonSet(p) {
				continue
			}
			if getPodPriority(p) >= getPodPriority(pending) {
				continue
			}
			if p.Spec.NodeName == cd.name {
				destPods = append(destPods, p)
			}
		}
		// Move biggest first to free space quickly
		sort.Slice(destPods, func(i, j int) bool {
			return getPodCPURequest(destPods[i]) > getPodCPURequest(destPods[j])
		})

		for _, dp := range destPods {
			if moveBudget <= 0 {
				break
			}
			// try moving dp to some *other* node (not target, not cd.name)
			excl := map[string]bool{cd.name: true, target: true}
			tgt := pl.bestDestExcluding(dp, loc, excl)
			if tgt == "" {
				continue
			}
			mv := PodMovement{Pod: dp, FromNode: cd.name, ToNode: tgt, CPURequest: getPodCPURequest(dp)}
			applyMove(loc, mv)
			plan = append(plan, mv)
			moveBudget--

			if loc[cd.name].AvailableCPU >= needCPU && loc[cd.name].AvailableMemory >= needMem {
				return cd.name, plan
			}
		}
	}
	return "", nil
}

// ---------------------------- Node selection helpers ----------------------------

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

func (pl *MyCrossNodePreemptionBinpacking) getMovablePods(pods []*v1.Pod, target *v1.Pod) []*v1.Pod {
	var out []*v1.Pod
	for _, p := range pods {
		if p.Namespace == target.Namespace && p.Name == target.Name {
			continue
		}
		if p.Namespace == "kube-system" || controlledByDaemonSet(p) {
			continue
		}
		if getPodPriority(p) < getPodPriority(target) {
			out = append(out, p)
		}
	}
	return out
}

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

func controlledByDaemonSet(p *v1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

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
	return name == "mycluster-control-plane" ||
		strings.Contains(name, "control-plane") ||
		strings.Contains(name, "master")
}
func isNodeSchedulable(st *NodeResourceState) bool {
	return st.AllocatableCPU > 0 && st.AllocatableMemory > 0
}
