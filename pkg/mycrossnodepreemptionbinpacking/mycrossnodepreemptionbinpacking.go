package mycrossnodepreemptionbinpacking

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// MyCrossNodePreemptionBinpacking implements cross-node preemption with bin-packing optimization
type MyCrossNodePreemptionBinpacking struct {
	handle        framework.Handle
	client        kubernetes.Interface
	args          *Config
	mu            sync.Mutex
	processedPods map[string]int // tries per pod UID
}

const maxPostFilterTries = 3

// Config holds the plugin configuration
type Config struct {
	MaxCandidates      int           `json:"maxCandidates,omitempty"`
	EnableBinPacking   bool          `json:"enableBinPacking,omitempty"`
	ConsiderPDBs       bool          `json:"considerPDBs,omitempty"`
	TimeoutDuration    time.Duration `json:"timeoutDuration,omitempty"`
	MinUtilizationGain float64       `json:"minUtilizationGain,omitempty"`
	MaxMovesPerPod     int           `json:"maxMovesPerPod,omitempty"`
}

// Name returns the plugin name
const Name = "MyCrossNodePreemptionBinpacking"

// Version identifier to track plugin updates
const Version = "v1.11.0"

// Name returns the plugin name
func (pl *MyCrossNodePreemptionBinpacking) Name() string {
	return Name
}

// New creates a new MyCrossNodePreemptionBinpacking plugin
func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	config := &Config{
		MaxCandidates:      100,
		EnableBinPacking:   true,
		ConsiderPDBs:       true,
		TimeoutDuration:    5 * time.Second,
		MinUtilizationGain: 0.1, // Require at least 10% better utilization
		MaxMovesPerPod:     5,   // Maximum number of pod movements to consider
	}

	if obj != nil {
		klog.V(2).InfoS("MyCrossNodePreemptionBinpacking plugin configuration", "config", obj)
	}

	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	klog.InfoS("MyCrossNodePreemptionBinpacking plugin successfully loaded and initialized",
		"version", Version,
		"maxCandidates", config.MaxCandidates,
		"enableBinPacking", config.EnableBinPacking,
		"maxMovesPerPod", config.MaxMovesPerPod)

	return &MyCrossNodePreemptionBinpacking{
		handle:        h,
		client:        client,
		args:          config,
		processedPods: make(map[string]int),
	}, nil
}

// PostFilter implements the PostFilter extension point for cross-node preemption with bin-packing
func (pl *MyCrossNodePreemptionBinpacking) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	filteredNodeStatusMap framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {
	defer func() {
		klog.V(2).InfoS("MyCrossNodePreemptionBinpacking PostFilter completed", "pod", klog.KObj(pod))
	}()

	klog.InfoS("MyCrossNodePreemptionBinpacking PostFilter started",
		"version", Version,
		"pod", klog.KObj(pod),
		"schedulerName", pod.Spec.SchedulerName,
		"priorityClass", pod.Spec.PriorityClassName)

	podKey := string(pod.UID)
	// bounded attempts
	pl.mu.Lock()
	tries := pl.processedPods[podKey]
	if tries >= maxPostFilterTries {
		pl.mu.Unlock()
		klog.V(2).InfoS("Pod already processed max times, skipping", "pod", klog.KObj(pod), "tries", tries)
		return nil, framework.NewStatus(framework.Unschedulable, "already processed")
	}
	pl.processedPods[podKey] = tries + 1
	pl.mu.Unlock()

	allNodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "Failed to list node infos")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

	if len(allNodes) == 0 {
		klog.V(2).InfoS("No nodes available for preemption")
		return nil, framework.NewStatus(framework.Unschedulable, "no nodes available")
	}

	candidates := pl.buildCandidates(allNodes, pod)
	solution := pl.findGlobalPreemptionStrategy(candidates, pod)
	if solution == nil {
		klog.V(2).InfoS("No viable bin-packing solution found", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.Unschedulable, "no preemption candidates found")
	}

	klog.V(2).InfoS("Found optimal bin-packing solution",
		"pod", klog.KObj(pod),
		"targetNode", solution.TargetNode,
		"podsToMove", len(solution.PodMovements),
		"podsToEvict", len(solution.VictimsToEvict),
		"utilizationGain", solution.UtilizationGain)

	if err := pl.executeBinPackingSolution(ctx, solution); err != nil {
		klog.ErrorS(err, "Failed to execute bin-packing solution")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}
	// cleanup record
	pl.mu.Lock()
	delete(pl.processedPods, podKey)
	pl.mu.Unlock()

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{
			NominatedNodeName: solution.TargetNode,
		},
	}, framework.NewStatus(framework.Success, "")
}

func (pl *MyCrossNodePreemptionBinpacking) buildCandidates(
	allNodes []*framework.NodeInfo,
	pod *v1.Pod,
) []*Candidate {
	var candidates []*Candidate

	for _, node := range allNodes {
		if isControlPlaneNode(node.Node().Name) {
			continue
		}
		availCPU := node.Allocatable.MilliCPU - node.Requested.MilliCPU
		if availCPU < 0 {
			availCPU = 0
		}
		availMem := node.Allocatable.Memory - node.Requested.Memory
		if availMem < 0 {
			availMem = 0
		}
		state := &NodeResourceState{
			Name:              node.Node().Name,
			AvailableCPU:      availCPU,
			AvailableMemory:   availMem,
			AllocatableCPU:    node.Allocatable.MilliCPU,
			AllocatableMemory: node.Allocatable.Memory,
			Pods:              []*v1.Pod{},
			UtilizationCPU:    0.0,
		}
		var lowPriorityPods []*v1.Pod
		for _, p := range node.Pods {
			state.Pods = append(state.Pods, p.Pod)
			if p.Pod.DeletionTimestamp == nil && getPodPriority(p.Pod) < getPodPriority(pod) {
				lowPriorityPods = append(lowPriorityPods, p.Pod)
			}
		}
		candidates = append(candidates, &Candidate{
			NodeName:  node.Node().Name,
			NodeState: state,
			Victims:   lowPriorityPods,
			Score:     0,
		})
	}

	return candidates
}

// BinPackingSolution represents an optimal solution for cross-node bin-packing
type BinPackingSolution struct {
	TargetNode      string
	PodMovements    []PodMovement
	VictimsToEvict  []*v1.Pod
	UtilizationGain float64
	TotalCPUSaved   int64
}

// PodMovement represents moving a pod from one node to another
type PodMovement struct {
	Pod        *v1.Pod
	FromNode   string
	ToNode     string
	CPURequest int64
}

type Candidate struct {
	NodeInfo *framework.NodeInfo
	Pods     []*v1.Pod

	NodeName  string
	NodeState *NodeResourceState
	Victims   []*v1.Pod
	Score     int64
}

// NodeResourceState tracks the resource state of a node during optimization
type NodeResourceState struct {
	Name              string
	AvailableCPU      int64
	AvailableMemory   int64
	AllocatableCPU    int64
	AllocatableMemory int64
	Pods              []*v1.Pod
	UtilizationCPU    float64
}

// ----- simulation helpers (core fix) -----

func removePod(list []*v1.Pod, p *v1.Pod) []*v1.Pod {
	out := list[:0]
	for _, x := range list {
		if !(x.UID == p.UID) {
			out = append(out, x)
		}
	}
	return out
}

func addPod(list []*v1.Pod, p *v1.Pod) []*v1.Pod {
	return append(list, p)
}

func applyMove(sim map[string]*NodeResourceState, mv PodMovement) {
	cpu := mv.CPURequest
	mem := getPodMemoryRequest(mv.Pod)
	// capacity deltas
	sim[mv.FromNode].AvailableCPU += cpu
	sim[mv.FromNode].AvailableMemory += mem
	sim[mv.ToNode].AvailableCPU -= cpu
	sim[mv.ToNode].AvailableMemory -= mem
	// pod lists
	sim[mv.FromNode].Pods = removePod(sim[mv.FromNode].Pods, mv.Pod)
	sim[mv.ToNode].Pods = addPod(sim[mv.ToNode].Pods, mv.Pod)
}

// findGlobalPreemptionStrategy finds the globally optimal pod movement and preemption plan
func (pl *MyCrossNodePreemptionBinpacking) findGlobalPreemptionStrategy(
	candidates []*Candidate,
	pod *v1.Pod,
) *BinPackingSolution {
	var bestPlan *BinPackingSolution
	var maxUtilizationGain float64

	allNodes := make(map[string]*NodeResourceState)
	for _, c := range candidates {
		allNodes[c.NodeName] = c.NodeState
	}

	for _, candidate := range candidates {
		if isControlPlaneNode(candidate.NodeName) {
			continue
		}
		cpuDef := getPodCPURequest(pod) - candidate.NodeState.AvailableCPU
		memDef := getPodMemoryRequest(pod) - candidate.NodeState.AvailableMemory
		if cpuDef < 0 {
			cpuDef = 0
		}
		if memDef < 0 {
			memDef = 0
		}

		plan := pl.findBinPackingSolutionForNode(
			context.TODO(), pod, candidate.NodeName, cpuDef, memDef, allNodes,
		)
		if plan != nil {
			better := false
			if bestPlan == nil {
				better = true
			} else if len(plan.VictimsToEvict) < len(bestPlan.VictimsToEvict) {
				better = true
			} else if len(plan.VictimsToEvict) == len(bestPlan.VictimsToEvict) &&
				plan.UtilizationGain > maxUtilizationGain {
				better = true
			}
			if better {
				klog.V(3).InfoS("New best plan found",
					"targetNode", candidate.NodeName,
					"evictions", len(plan.VictimsToEvict),
					"gain", plan.UtilizationGain)
				maxUtilizationGain = plan.UtilizationGain
				bestPlan = plan
			}
		}
	}
	return bestPlan
}

// findBinPackingSolutionForNode finds bin-packing solution for a specific target node
func (pl *MyCrossNodePreemptionBinpacking) findBinPackingSolutionForNode(
	ctx context.Context,
	pod *v1.Pod,
	targetNodeName string,
	cpuDeficit, memoryDeficit int64,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {
	targetState := nodeStates[targetNodeName]
	if targetState == nil {
		klog.Infof("Target node %q missing from nodeStates", targetNodeName)
		return nil
	}

	movablePods := pl.getMovablePods(targetState.Pods, pod)
	klog.InfoS("Collected movable pods", "count", len(movablePods))

	klog.V(4).InfoS("Finding bin-packing solution for target node",
		"targetNode", targetNodeName,
		"movablePods", len(movablePods),
		"cpuDeficit", cpuDeficit,
		"memoryDeficit", memoryDeficit)

	// Try both orders; pick the better plan.
	return pl.optimizeBinPacking(pod, targetNodeName, cpuDeficit, memoryDeficit, movablePods, nodeStates)
}

// chooseDestWithHelpers tries plain fit first; if it doesn't fit,
// it attempts up to `depth` helper moves on the destination to free room.
func (pl *MyCrossNodePreemptionBinpacking) chooseDestWithHelpers(
	relocating *v1.Pod,
	pending *v1.Pod,
	targetNode string, // the *target* for the pending pod; must be excluded
	sim map[string]*NodeResourceState,
	depth int,
	maxMoves int,
) (string, []PodMovement) {

	needCPU := getPodCPURequest(relocating)
	needMem := getPodMemoryRequest(relocating)

	// 1) Plain fit (exclude targetNode)
	excl := map[string]bool{targetNode: true}
	if dest := pl.findBestDestinationNodeExcluding(relocating, sim, excl); dest != "" {
		return dest, nil
	}
	if depth <= 0 || maxMoves <= 0 {
		return "", nil
	}

	// 2) Try candidate destinations that are "close" and free them using helper moves
	type cand struct{ name string; cpuGap, memGap int64 }
	var cands []cand
	for name, st := range sim {
		if name == targetNode || isControlPlaneNode(name) || !isNodeSchedulable(st) {
			continue
		}
		cpuGap := needCPU - st.AvailableCPU
		memGap := needMem - st.AvailableMemory
		if cpuGap > 0 || memGap > 0 {
			if cpuGap <= needCPU && memGap <= needMem {
				cands = append(cands, cand{name, cpuGap, memGap})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].cpuGap < cands[j].cpuGap })

	for _, cd := range cands {
		// local snapshot
		loc := pl.copyNodeStates(sim)
		var plan []PodMovement

		// Movable pods on cd.name (lower priority than pending)
		var destPods []*v1.Pod
		for _, p := range loc[cd.name].Pods {
			if p.Spec.NodeName != cd.name {
				continue
			}
			if p.Namespace == "kube-system" || controlledByDaemonSet(p) {
				continue
			}
			if getPodPriority(p) >= getPodPriority(pending) {
				continue
			}
			destPods = append(destPods, p)
		}
		sort.Slice(destPods, func(i, j int) bool {
			return getPodCPURequest(destPods[i]) > getPodCPURequest(destPods[j])
		})

		for _, dp := range destPods {
			if len(plan) >= maxMoves {
				break
			}

			// Try plain fit to any node excluding cd.name and targetNode
			excl2 := map[string]bool{cd.name: true, targetNode: true}
			tgt := pl.findBestDestinationNodeExcluding(dp, loc, excl2)
			var subhelpers []PodMovement

			// If no plain fit and we still have depth, try freeing *some* node recursively.
			if tgt == "" && depth > 1 && maxMoves-len(plan) > 0 {
				if dest2, helpers2 := pl.chooseDestWithHelpers(
					dp, pending, targetNode, loc, depth-1, maxMoves-len(plan),
				); dest2 != "" && dest2 != cd.name && dest2 != targetNode {
					tgt = dest2
					subhelpers = helpers2
				}
			}
			if tgt == "" {
				continue
			}

			// apply subhelpers first
			for _, mv := range subhelpers {
				applyMove(loc, mv)
				plan = append(plan, mv)
				if len(plan) >= maxMoves {
					break
				}
			}
			if len(plan) >= maxMoves {
				break
			}

			// move dp off cd.name to tgt
			mv := PodMovement{
				Pod:        dp,
				FromNode:   cd.name,
				ToNode:     tgt,
				CPURequest: getPodCPURequest(dp),
			}
			applyMove(loc, mv)
			plan = append(plan, mv)

			// enough room now?
			if loc[cd.name].AvailableCPU >= needCPU && loc[cd.name].AvailableMemory >= needMem {
				return cd.name, plan
			}
		}
	}
	return "", nil
}

// optimizeBinPacking tries both orders (DESC/ASC by CPU) and picks the better plan.
func (pl *MyCrossNodePreemptionBinpacking) optimizeBinPacking(
	pending *v1.Pod, // pending high-priority pod
	targetNodeName string,
	cpuDeficit, memoryDeficit int64,
	movablePods []*v1.Pod,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {
	// 1) largest-first
	podsDesc := append([]*v1.Pod(nil), movablePods...)
	sort.Slice(podsDesc, func(i, j int) bool {
		return getPodCPURequest(podsDesc[i]) > getPodCPURequest(podsDesc[j])
	})
	planA := pl.optimizeOnce(pending, targetNodeName, cpuDeficit, memoryDeficit, podsDesc, nodeStates)

	// 2) smallest-first
	podsAsc := append([]*v1.Pod(nil), movablePods...)
	sort.Slice(podsAsc, func(i, j int) bool {
		return getPodCPURequest(podsAsc[i]) < getPodCPURequest(podsAsc[j])
	})
	planB := pl.optimizeOnce(pending, targetNodeName, cpuDeficit, memoryDeficit, podsAsc, nodeStates)

	return pl.pickBetterPlan(planA, planB)
}

// optimizeOnce uses a greedy approach for a fixed pod order.
// Evictions are DEFERRED: try all moves (inc. helper moves) first; then evict smallest set if needed.
func (pl *MyCrossNodePreemptionBinpacking) optimizeOnce(
	pending *v1.Pod,
	targetNodeName string,
	cpuDeficit, memoryDeficit int64,
	orderedMovable []*v1.Pod,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {

	klog.InfoS("Trying to optimize bin-packing (single-order)",
		"targetNode", targetNodeName,
		"movablePods", len(orderedMovable),
		"cpuDeficit", cpuDeficit,
		"memoryDeficit", memoryDeficit)

	bestSolution := &BinPackingSolution{
		TargetNode:      targetNodeName,
		PodMovements:    []PodMovement{},
		VictimsToEvict:  []*v1.Pod{},
		UtilizationGain: 0,
	}

	// Create a copy of node states for simulation
	simStates := pl.copyNodeStates(nodeStates)

	freedCPU := int64(0)
	freedMemory := int64(0)
	totalMovedCPU := int64(0)

	// Defer potential evictions until after we try all moves.
	var couldntPlace []*v1.Pod

	for _, podToMove := range orderedMovable {
		if len(bestSolution.PodMovements) >= pl.args.MaxMovesPerPod {
			klog.V(3).InfoS("Move budget exhausted; deferring remaining pods", "budget", pl.args.MaxMovesPerPod)
			couldntPlace = append(couldntPlace, podToMove)
			continue
		}

		podCPU := getPodCPURequest(podToMove)
		podMemory := getPodMemoryRequest(podToMove)

		// Only try to move pods that are actually ON the target node
		if podToMove.Spec.NodeName != targetNodeName {
			klog.V(4).InfoS("Skipping pod movement - pod is not on target node",
				"pod", klog.KObj(podToMove),
				"podCurrentNode", podToMove.Spec.NodeName,
				"targetNode", targetNodeName)
			continue
		}

		// Try to find a destination node for this pod (AWAY from target node)
		destNode := pl.findBestDestinationNode(podToMove, targetNodeName, simStates)
		helperMoves := []PodMovement{}
		if destNode == "" {
			destNode, helperMoves = pl.chooseDestWithHelpers(
				podToMove, pending, targetNodeName, simStates,
				/*depth=*/2,
				/*maxMoves=*/pl.args.MaxMovesPerPod-len(bestSolution.PodMovements)-1,
			)
			if destNode == "" {
				klog.V(3).InfoS("No destination found for movable pod; deferring eviction",
					"pod", klog.KObj(podToMove),
					"targetNode", targetNodeName,
					"helperDepth", 2,
					"movesUsed", len(bestSolution.PodMovements),
					"moveBudget", pl.args.MaxMovesPerPod)
				couldntPlace = append(couldntPlace, podToMove)
				continue
			}
		}

		neededMoves := len(helperMoves) + 1 // helpers + the main move
		if destNode != "" &&
			destNode != targetNodeName &&
			len(bestSolution.PodMovements)+neededMoves <= pl.args.MaxMovesPerPod {

			// apply helper moves now (first real mutation of simStates)
			for _, mv := range helperMoves {
				applyMove(simStates, mv)
				bestSolution.PodMovements = append(bestSolution.PodMovements, mv)
				totalMovedCPU += mv.CPURequest
			}

			// move the original pod off target
			mv := PodMovement{Pod: podToMove, FromNode: podToMove.Spec.NodeName, ToNode: destNode, CPURequest: podCPU}
			applyMove(simStates, mv)
			bestSolution.PodMovements = append(bestSolution.PodMovements, mv)

			freedCPU += podCPU
			freedMemory += podMemory
			totalMovedCPU += podCPU
		} else {
			// destination found but would exceed move budget — defer
			klog.V(3).InfoS("Move would exceed budget; deferring eviction",
				"pod", klog.KObj(podToMove),
				"neededMoves", neededMoves,
				"usedMoves", len(bestSolution.PodMovements),
				"budget", pl.args.MaxMovesPerPod)
			couldntPlace = append(couldntPlace, podToMove)
		}

		// stop early if deficits covered
		if freedCPU >= cpuDeficit && freedMemory >= memoryDeficit {
			break
		}
	}

	// If still short, evict the minimum number of smallest-CPU pods.
	if freedCPU < cpuDeficit || freedMemory < memoryDeficit {
		sort.Slice(couldntPlace, func(i, j int) bool {
			return getPodCPURequest(couldntPlace[i]) < getPodCPURequest(couldntPlace[j])
		})
		for _, p := range couldntPlace {
			if freedCPU >= cpuDeficit && freedMemory >= memoryDeficit {
				break
			}
			bestSolution.VictimsToEvict = append(bestSolution.VictimsToEvict, p)
			freedCPU += getPodCPURequest(p)
			freedMemory += getPodMemoryRequest(p)
			klog.V(3).InfoS("Evicting pod to meet deficit",
				"pod", klog.KObj(p),
				"freedCPU", freedCPU, "cpuDeficit", cpuDeficit,
				"freedMem", freedMemory, "memDeficit", memoryDeficit)
		}
		if freedCPU < cpuDeficit || freedMemory < memoryDeficit {
			klog.InfoS("Failed to free enough resources after moves+evictions",
				"targetNode", targetNodeName,
				"freedCPU", freedCPU, "cpuDeficit", cpuDeficit,
				"freedMemory", freedMemory, "memoryDeficit", memoryDeficit)
			return nil
		}
	}

	// Calculate utilization gain; penalize evictions so move-only plans win.
	evictedCPU := int64(0)
	for _, victim := range bestSolution.VictimsToEvict {
		evictedCPU += getPodCPURequest(victim)
	}
	evictionPenalty := float64(3) // tunable
	den := float64(totalMovedCPU) + evictionPenalty*float64(len(bestSolution.VictimsToEvict))*1000.0
	if den > 0 {
		bestSolution.UtilizationGain = float64(totalMovedCPU) / den
	} else {
		bestSolution.UtilizationGain = 0
	}
	bestSolution.TotalCPUSaved = evictedCPU

	klog.V(4).InfoS("Bin-packing solution calculated (single-order)",
		"targetNode", targetNodeName,
		"movements", len(bestSolution.PodMovements),
		"evictions", len(bestSolution.VictimsToEvict),
		"utilizationGain", bestSolution.UtilizationGain,
		"cpuSavedByEviction(mCPU)", bestSolution.TotalCPUSaved)

	return bestSolution
}

// pickBetterPlan prefers: (1) non-nil; (2) fewer evictions; (3) higher UtilizationGain.
func (pl *MyCrossNodePreemptionBinpacking) pickBetterPlan(a, b *BinPackingSolution) *BinPackingSolution {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if len(a.VictimsToEvict) != len(b.VictimsToEvict) {
		if len(a.VictimsToEvict) < len(b.VictimsToEvict) {
			return a
		}
		return b
	}
	if a.UtilizationGain >= b.UtilizationGain {
		return a
	}
	return b
}

func (pl *MyCrossNodePreemptionBinpacking) findBestDestinationNodeExcluding(
	pod *v1.Pod,
	nodeStates map[string]*NodeResourceState,
	exclude map[string]bool,
) string {
	podCPU := getPodCPURequest(pod)
	podMemory := getPodMemoryRequest(pod)
	var best string
	bestUtil := 2.0
	for name, st := range nodeStates {
		if exclude[name] || isControlPlaneNode(name) || !isNodeSchedulable(st) {
			continue
		}
		if st.AvailableCPU >= podCPU && st.AvailableMemory >= podMemory {
			newUsed := (st.AllocatableCPU - st.AvailableCPU) + podCPU
			util := float64(newUsed) / float64(st.AllocatableCPU)
			if util < bestUtil {
				best = name
				bestUtil = util
			}
		}
	}
	return best
}

// findBestDestinationNode finds the best node to move a pod to
func (pl *MyCrossNodePreemptionBinpacking) findBestDestinationNode(
	pod *v1.Pod,
	excludeNode string,
	nodeStates map[string]*NodeResourceState,
) string {

	podCPU := getPodCPURequest(pod)
	podMemory := getPodMemoryRequest(pod)

	var bestNode string
	bestUtilization := float64(1.1) // Start higher than 100%

	for nodeName, state := range nodeStates {
		if nodeName == excludeNode {
			continue
		}
		if isControlPlaneNode(nodeName) || !isNodeSchedulable(state) {
			continue
		}
		if state.AvailableCPU >= podCPU && state.AvailableMemory >= podMemory {
			newCPUUsed := (state.AllocatableCPU - state.AvailableCPU) + podCPU
			newUtilization := float64(newCPUUsed) / float64(state.AllocatableCPU)
			if newUtilization < bestUtilization {
				bestNode = nodeName
				bestUtilization = newUtilization
			}
		}
	}
	return bestNode
}

func controlledByDaemonSet(p *v1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func (pl *MyCrossNodePreemptionBinpacking) getMovablePods(pods []*v1.Pod, target *v1.Pod) []*v1.Pod {
	var movable []*v1.Pod
	for _, pod := range pods {
		klog.Infof("Evaluating pod %s/%s: priority %d vs target %d",
			pod.Namespace, pod.Name, getPodPriority(pod), getPodPriority(target))

		if pod.Namespace == target.Namespace && pod.Name == target.Name {
			klog.Infof("Skipping self pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		if pod.Namespace == "kube-system" || controlledByDaemonSet(pod) {
			continue
		}
		if getPodPriority(pod) < getPodPriority(target) {
			klog.Infof("Movable: %s/%s (priority %d < %d)",
				pod.Namespace, pod.Name, getPodPriority(pod), getPodPriority(target))
			movable = append(movable, pod)
		} else {
			klog.Infof("Not movable: %s/%s (priority %d >= %d)",
				pod.Namespace, pod.Name, getPodPriority(pod), getPodPriority(target))
		}
	}
	return movable
}

func (pl *MyCrossNodePreemptionBinpacking) copyNodeStates(original map[string]*NodeResourceState) map[string]*NodeResourceState {
	out := make(map[string]*NodeResourceState, len(original))
	for name, st := range original {
		pods := make([]*v1.Pod, len(st.Pods))
		copy(pods, st.Pods)

		out[name] = &NodeResourceState{
			Name:              st.Name,
			AvailableCPU:      st.AvailableCPU,
			AvailableMemory:   st.AvailableMemory,
			AllocatableCPU:    st.AllocatableCPU,
			AllocatableMemory: st.AllocatableMemory,
			Pods:              pods,
			UtilizationCPU:    st.UtilizationCPU,
		}
	}
	return out
}

func getPodCPURequest(pod *v1.Pod) int64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if req := container.Resources.Requests[v1.ResourceCPU]; !req.IsZero() {
			total += req.MilliValue()
		}
	}
	return total
}

func getPodMemoryRequest(pod *v1.Pod) int64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if req := container.Resources.Requests[v1.ResourceMemory]; !req.IsZero() {
			total += req.Value()
		}
	}
	return total
}

func getPodPriority(pod *v1.Pod) int32 {
	if pod.Spec.Priority != nil {
		return *pod.Spec.Priority
	}
	return 0
}

// isControlPlaneNode checks if a node is a control-plane node
func isControlPlaneNode(nodeName string) bool {
	return nodeName == "mycluster-control-plane" ||
		strings.Contains(nodeName, "control-plane") ||
		strings.Contains(nodeName, "master")
}

// isNodeSchedulable checks if a node can accept new pods
func isNodeSchedulable(state *NodeResourceState) bool {
	return state.AllocatableCPU > 0 && state.AllocatableMemory > 0
}
