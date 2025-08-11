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
	Version            = "v1.15.0"
	maxPostFilterTries = 3
)

type Config struct {
	MaxMovesPerPod int `json:"maxMovesPerPod,omitempty"`
}

// ---------------------------- Initialization ----------------------------
// Functions used by main scheduler to identify the plugin.

// Name returns the name of the plugin.
func (pl *MyCrossNodePreemptionBinpacking) Name() string { return Name }

// New creates a new instance of the plugin.
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

// Implements the PostFilter interface for the plugin.
func (pl *MyCrossNodePreemptionBinpacking) PostFilter(ctx context.Context, state *framework.CycleState, pendingPod *v1.Pod, _ framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
    klog.InfoS("PostFilter start", "pod", klog.KObj(pendingPod))

	// Get all nodes incl. resource stats
    nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
    if err != nil || len(nodes) == 0 {
        if err != nil { klog.ErrorS(err, "list nodes") }
        return nil, framework.NewStatus(framework.Unschedulable, "no nodes")
    }

    candidateNodes := pl.buildPreemptionCandidates(nodes, pendingPod) // candidates for possible preemption
    plan := pl.findPlan(candidateNodes, pendingPod) // find the best bin-packing solution

	// If no plan found the PostFilter will return unschedulable status.
	if plan == nil { return nil, framework.NewStatus(framework.Unschedulable, "no preemption candidates found") }

	// Execute the bin-packing solution
	if err := pl.executeBinPackingPlan(ctx, plan); err != nil {
        klog.ErrorS(err, "plan execution failed")
        return nil, framework.NewStatus(framework.Error, err.Error())
    }
	
	// Update the nominated node (i.e. target node where the new pod will be scheduled)
    return &framework.PostFilterResult{
        NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.TargetNode},
    }, framework.NewStatus(framework.Success, "")
}

// ---------------------------- Modeling ----------------------------

type CandidateNode struct {
	NodeName  string
	State     *NodeResourceState
	Victims   []*v1.Pod
}

type NodeResourceState struct {
	Name              string
	AvailableCPU      int64 // milliCPU
	AvailableMemory   int64 // bytes
	AllocatableCPU    int64 // milliCPU
	AllocatableMemory int64 // bytes
	Pods              []*v1.Pod
}

type BinPackingPlan struct {
	TargetNode     string
	PodMovements   []PodMovement
	VictimsToEvict []*v1.Pod
}

type PodMovement struct {
	Pod           *v1.Pod
	FromNode      string
	ToNode     	  string
	CPURequest 	  int64 // milliCPU
	MemoryRequest int64 // bytes
}

// buildPreemptionCandidates creates a list of candidate nodes that has pods that can be preempted to make the
// pending pod schedulable.
func (pl *MyCrossNodePreemptionBinpacking) buildPreemptionCandidates(nodes []*framework.NodeInfo, pendingPod *v1.Pod) []*CandidateNode {

	var candidateNodes []*CandidateNode
	
	// Loop through all nodes
	for _, nodeInfo := range nodes {
		
		// Skip control plane nodes
		if isControlPlaneNode(nodeInfo.Node().Name) { continue }

		// Calculate available resources
		availCPU := nodeInfo.Allocatable.MilliCPU - nodeInfo.Requested.MilliCPU
		availMem := nodeInfo.Allocatable.Memory - nodeInfo.Requested.Memory
		if availCPU < 0 { availCPU = 0 }
		if availMem < 0 { availMem = 0 }

		// Create node resource state of this node
		nodeState := &NodeResourceState{
			Name:              nodeInfo.Node().Name,
			AvailableCPU:      availCPU,
			AvailableMemory:   availMem,
			AllocatableCPU:    nodeInfo.Allocatable.MilliCPU,
			AllocatableMemory: nodeInfo.Allocatable.Memory,
			Pods:              []*v1.Pod{},
		}

		// Identify low-priority pods that can be preempted of this node
		var lowPriorityPods []*v1.Pod
		for _, podInfo := range nodeInfo.Pods {
			nodeState.Pods = append(nodeState.Pods, podInfo.Pod)
			if podInfo.Pod.DeletionTimestamp == nil && getPodPriority(podInfo.Pod) < getPodPriority(pendingPod) {
				lowPriorityPods = append(lowPriorityPods, podInfo.Pod)
			}
		}

		candidateNodes = append(candidateNodes, &CandidateNode{NodeName: nodeInfo.Node().Name, State: nodeState, Victims: lowPriorityPods})
	}

	return candidateNodes
}

// ---------------------------- Planner ----------------------------

// findPlan identifies the best bin-packing plan from the available candidate nodes by evaluating their resource states and potential pod movements.
func (pl *MyCrossNodePreemptionBinpacking) findPlan(candidateNodes []*CandidateNode, pendingPod *v1.Pod) *BinPackingPlan {
	allNodeStates := map[string]*NodeResourceState{}
	for _, candidate := range candidateNodes {
		allNodeStates[candidate.NodeName] = candidate.State
	}

	var bestPlan *BinPackingPlan
	bestEvict := int(1 << 30) // largest int value
	bestMoves := int(1 << 30) // largest int value

	for _, candidateNode := range candidateNodes {
		if isControlPlaneNode(candidateNode.NodeName) { continue }
		cpuDef := getPodCPURequest(pendingPod) - candidateNode.State.AvailableCPU
		memDef := getPodMemoryRequest(pendingPod) - candidateNode.State.AvailableMemory
		if cpuDef < 0 { cpuDef = 0 }
		if memDef < 0 { memDef = 0 }

		candidateState := allNodeStates[candidateNode.NodeName]
		if candidateState == nil {
			return nil
		}

		movablePodsOfCandidateNode := pl.getMovablePods(candidateState.Pods, pendingPod)

		// Largest first tends to minimize #moves/evictions
		// sort movable pods by their CPU requests in descending order
		sort.Slice(movablePodsOfCandidateNode, func(i, j int) bool {
			return getPodCPURequest(movablePodsOfCandidateNode[i]) > getPodCPURequest(movablePodsOfCandidateNode[j])
		})

		// Try to move pods first, then evict if necessary
		plan := pl.moveThenEvict(pendingPod, candidateNode.NodeName, cpuDef, memDef, movablePodsOfCandidateNode, allNodeStates)
		if plan == nil { continue }

		// Check if this plan is better than the best found so far by evaluating the number of evictions and movements.
		// Fewer evictions are preferred first, then fewer movements.
		numOfEvictions, numOfMovements := len(plan.VictimsToEvict), len(plan.PodMovements)
		if numOfEvictions < bestEvict || (numOfEvictions == bestEvict && numOfMovements < bestMoves) {
			bestPlan, bestEvict, bestMoves = plan, numOfEvictions, numOfMovements
		}
	}
	return bestPlan
}

// Try moves first, then evict if necessary
func (pl *MyCrossNodePreemptionBinpacking) moveThenEvict(pendingPod *v1.Pod, targetNode string, cpuDef, memDef int64, movablePodsOfTargetNode []*v1.Pod, global map[string]*NodeResourceState) *BinPackingPlan {
	sim := pl.copyNodeStates(global) // create a copy of the global node states to avoid mutating the original
	plan := &BinPackingPlan{TargetNode: targetNode}

	var freedCPU, freedMem int64
	moveBudget := pl.args.MaxMovesPerPod
	var notMovablePods []*v1.Pod

	for _, pod := range movablePodsOfTargetNode {

		// Skip pods not on the target node
		if pod.Spec.NodeName != targetNode {
			continue
		}
		if moveBudget <= 0 { // no more moves allowed since the limit for this pod has been reached
			notMovablePods = append(notMovablePods, pod)
			continue
		}

		destinationNode := pl.bestDestinationNode(pod, sim, map[string]bool{targetNode: true}) // find best destination for the pod
		helperPods := []PodMovement{} // a helper is a pod movement that frees up space on the target node
		
		// Try to find a destination with one helper layer
		if destinationNode == "" && moveBudget > 1 {
			destinationNode, helperPods = pl.freeDestWithOneHelperLayer(pod, pendingPod, targetNode, sim, moveBudget-1)
		}
		// If no destination found, add to notMovablePods
		if destinationNode == "" {
			notMovablePods = append(notMovablePods, pod)
			continue
		}

		// Apply helper moves to free up space at the target node
		for _, mv := range helperPods {
			applyMove(sim, mv)
			plan.PodMovements = append(plan.PodMovements, mv)
			moveBudget--
		}

		// Move pod to destination
		mv := PodMovement{Pod: pod, FromNode: targetNode, ToNode: destinationNode, CPURequest: getPodCPURequest(pod)}
		applyMove(sim, mv)
		plan.PodMovements = append(plan.PodMovements, mv)
		moveBudget--

		// Track freed resources. If we have freed enough resources, we can stop
		freedCPU += getPodCPURequest(pod)
		freedMem += getPodMemoryRequest(pod)
		if freedCPU >= cpuDef && freedMem >= memDef { break }
	}

	// If the moves were not enough, we need now to consider eviction.
	// Therefore, we need to sort the notMovablePods by their CPU requests in ascending order.
	// The victims are the pods that we will consider for eviction.
	if freedCPU < cpuDef || freedMem < memDef {
		sort.Slice(notMovablePods, func(i, j int) bool {
			return getPodCPURequest(notMovablePods[i]) < getPodCPURequest(notMovablePods[j])
		})
		for _, pod := range notMovablePods {
			if freedCPU >= cpuDef && freedMem >= memDef { break }
			plan.VictimsToEvict = append(plan.VictimsToEvict, pod)
			freedCPU += getPodCPURequest(pod)
			freedMem += getPodMemoryRequest(pod)
		}
		if freedCPU < cpuDef || freedMem < memDef { return nil }
	}

	return plan
}

// freeDestWithOneHelperLayer frees a destination node by moving a single chain of pods off it.
func (pl *MyCrossNodePreemptionBinpacking) freeDestWithOneHelperLayer(relocating, pendingPod *v1.Pod, target string, sim map[string]*NodeResourceState, moveBudget int) (string, []PodMovement) {
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
			cands = append(cands, destCand{name, cpuGap, memGap})
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
			if getPodPriority(p) >= getPodPriority(pendingPod) { continue }
			if p.Spec.NodeName == dc.name { destPods = append(destPods, p) }
		}
		sort.Slice(destPods, func(i, j int) bool { return getPodCPURequest(destPods[i]) > getPodCPURequest(destPods[j]) })

		for _, dp := range destPods {
			if moveBudget <= 0 { break }
			excl := map[string]bool{dc.name: true, target: true}
			tgt := pl.bestDestinationNode(dp, loc, excl)
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

// bestDestinationNode finds the best destination node for a pod, excluding certain nodes
func (pl *MyCrossNodePreemptionBinpacking) bestDestinationNode(pod *v1.Pod, nodeStates map[string]*NodeResourceState, exclude map[string]bool,) string {
	podCPU := getPodCPURequest(pod)
	podMem := getPodMemoryRequest(pod)
	var bestDestinationNode string
	bestUtil := 2.0 // min utilization after placing the pod.
	
	for podName, podState := range nodeStates {
		if exclude[podName] || isControlPlaneNode(podName) || !isNodeSchedulable(podState) {
			continue
		}
		if podState.AvailableCPU >= podCPU && podState.AvailableMemory >= podMem {
			newUsed := (podState.AllocatableCPU - podState.AvailableCPU) + podCPU
			util := float64(newUsed) / float64(podState.AllocatableCPU)
			if util < bestUtil {
				bestDestinationNode, bestUtil = podName, util
			}
		}
	}
	return bestDestinationNode
}

// ---------------------------- Utilities ----------------------------

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
func (pl *MyCrossNodePreemptionBinpacking) getMovablePods(pods []*v1.Pod, targetPod *v1.Pod) []*v1.Pod {
	var movablePods []*v1.Pod
	for _, pod := range pods {
		// Skip the target pod itself
		if pod.Namespace == targetPod.Namespace && pod.Name == targetPod.Name {
			continue
		}
		// Skip kube-system namespace pods
		if pod.Namespace == "kube-system" {
			continue
		}
		// Only consider pods with lower priority than the target pod
		if getPodPriority(pod) < getPodPriority(targetPod) {
			movablePods = append(movablePods, pod)
		}
	}
	return movablePods
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