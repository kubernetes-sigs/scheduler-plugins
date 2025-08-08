package mycrossnodepreemptionbinpacking

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// MyCrossNodePreemptionBinpacking implements cross-node preemption with bin-packing optimization
type MyCrossNodePreemptionBinpacking struct {
	handle       framework.Handle
	client       kubernetes.Interface
	args         *Config
	processedPods map[string]bool // Track processed pods to avoid duplicates
}

// Config holds the plugin configuration
type Config struct {
	MaxCandidates       int           `json:"maxCandidates,omitempty"`
	EnableBinPacking    bool          `json:"enableBinPacking,omitempty"`
	ConsiderPDBs        bool          `json:"considerPDBs,omitempty"`
	TimeoutDuration     time.Duration `json:"timeoutDuration,omitempty"`
	MinUtilizationGain  float64       `json:"minUtilizationGain,omitempty"`
	MaxMovesPerPod      int           `json:"maxMovesPerPod,omitempty"`
}

// Name returns the plugin name
const Name = "MyCrossNodePreemptionBinpacking"

// Version identifier to track plugin updates
const Version = "v1.3.0-debug-movable-pods"

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
		// Parse configuration if provided
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
		processedPods: make(map[string]bool),
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

	// Check if we've already processed this pod to avoid multiple executions
	podKey := string(pod.UID)
	if pl.processedPods[podKey] {
		klog.V(2).InfoS("Pod already processed, skipping duplicate execution", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.Unschedulable, "already processed")
	}
	pl.processedPods[podKey] = true

	// Get all nodes for cross-node analysis
	allNodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "Failed to list node infos")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

	if len(allNodes) == 0 {
		klog.V(2).InfoS("No nodes available for preemption")
		return nil, framework.NewStatus(framework.Unschedulable, "no nodes available")
	}

	// Find optimal bin-packing solution across all nodes
	solution, err := pl.findOptimalBinPackingSolution(ctx, pod, allNodes, filteredNodeStatusMap)
	if err != nil {
		klog.ErrorS(err, "Failed to find optimal bin-packing solution")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

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

	// Execute the bin-packing solution
	err = pl.executeBinPackingSolution(ctx, solution)
	if err != nil {
		klog.ErrorS(err, "Failed to execute bin-packing solution")
		return nil, framework.NewStatus(framework.Error, err.Error())
	}

	// Return the nominated node for the pod
	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{
			NominatedNodeName: solution.TargetNode,
		},
	}, framework.NewStatus(framework.Success, "")
}

// BinPackingSolution represents an optimal solution for cross-node bin-packing
type BinPackingSolution struct {
	TargetNode       string
	PodMovements     []PodMovement
	VictimsToEvict   []*v1.Pod
	UtilizationGain  float64
	TotalCPUSaved    int64
}

// PodMovement represents moving a pod from one node to another
type PodMovement struct {
	Pod        *v1.Pod
	FromNode   string
	ToNode     string
	CPURequest int64
}

// NodeResourceState tracks the resource state of a node during optimization
type NodeResourceState struct {
	Name               string
	AvailableCPU       int64
	AvailableMemory    int64
	AllocatableCPU     int64
	AllocatableMemory  int64
	Pods               []*v1.Pod
	UtilizationCPU     float64
}

// findOptimalBinPackingSolution finds the best cross-node bin-packing solution
func (pl *MyCrossNodePreemptionBinpacking) findOptimalBinPackingSolution(
	ctx context.Context,
	pod *v1.Pod,
	allNodes []*framework.NodeInfo,
	filteredNodeStatusMap framework.NodeToStatusMap,
) (*BinPackingSolution, error) {
	
	podCPURequest := getPodCPURequest(pod)
	podMemoryRequest := getPodMemoryRequest(pod)
	
	klog.V(3).InfoS("Analyzing bin-packing opportunities",
		"podCPU", podCPURequest,
		"podMemory", podMemoryRequest,
		"totalNodes", len(allNodes))

	// Create initial node resource states
	nodeStates := make(map[string]*NodeResourceState)
	for _, nodeInfo := range allNodes {
		state := &NodeResourceState{
			Name:               nodeInfo.Node().Name,
			AllocatableCPU:     nodeInfo.Allocatable.MilliCPU,
			AllocatableMemory:  nodeInfo.Allocatable.Memory,
			AvailableCPU:       nodeInfo.Allocatable.MilliCPU - nodeInfo.Requested.MilliCPU,
			AvailableMemory:    nodeInfo.Allocatable.Memory - nodeInfo.Requested.Memory,
			Pods:               make([]*v1.Pod, 0),
		}
		
		// Collect pods from this node
		for _, podInfo := range nodeInfo.Pods {
			if podInfo.Pod.DeletionTimestamp == nil && 
			   getPodPriority(podInfo.Pod) < getPodPriority(pod) {
				state.Pods = append(state.Pods, podInfo.Pod)
			}
		}
		
		state.UtilizationCPU = float64(nodeInfo.Requested.MilliCPU) / float64(nodeInfo.Allocatable.MilliCPU)
		nodeStates[nodeInfo.Node().Name] = state
	}

	var bestSolution *BinPackingSolution
	bestUtilizationGain := pl.args.MinUtilizationGain
	bestCpuDeficit := int64(999999) // Prefer nodes that need less preemption

	// Try each node as the target node for the high-priority pod
	for targetNodeName := range nodeStates {
		// Skip nodes that can already fit the pod without preemption
		// We'll check node fitness in our custom logic
		
		klog.V(4).InfoS("Evaluating target node", 
			"node", targetNodeName,
			"availableCPU", nodeStates[targetNodeName].AvailableCPU,
			"requiredCPU", podCPURequest)

		// Calculate how much CPU we need to free up on target node
		targetState := nodeStates[targetNodeName]
		cpuDeficit := podCPURequest - targetState.AvailableCPU
		memoryDeficit := podMemoryRequest - targetState.AvailableMemory

		if cpuDeficit <= 0 && memoryDeficit <= 0 {
			// Already fits, no preemption needed
			continue
		}

		// Find bin-packing solution for this target node
		solution := pl.findBinPackingSolutionForNode(ctx, pod, targetNodeName, cpuDeficit, memoryDeficit, nodeStates)
		if solution != nil {
			// Prioritize solutions that need less preemption (smaller CPU deficit)
			// and have better utilization gain
			if cpuDeficit < bestCpuDeficit || 
			   (cpuDeficit == bestCpuDeficit && solution.UtilizationGain > bestUtilizationGain) {
				bestSolution = solution
				bestUtilizationGain = solution.UtilizationGain
				bestCpuDeficit = cpuDeficit
				klog.V(3).InfoS("Found better bin-packing solution",
					"targetNode", targetNodeName,
					"cpuDeficit", cpuDeficit,
					"utilizationGain", solution.UtilizationGain,
					"movements", len(solution.PodMovements),
					"evictions", len(solution.VictimsToEvict))
			}
		}
	}

	return bestSolution, nil
}

// findBinPackingSolutionForNode finds bin-packing solution for a specific target node
func (pl *MyCrossNodePreemptionBinpacking) findBinPackingSolutionForNode(
	ctx context.Context,
	pod *v1.Pod,
	targetNodeName string,
	cpuDeficit, memoryDeficit int64,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {
	
	// Get pods from target node that can be moved or evicted
	targetState := nodeStates[targetNodeName]
	movablePods := pl.getMovablePods(targetState.Pods, getPodPriority(pod))
	
	klog.V(4).InfoS("Finding bin-packing solution for target node",
		"targetNode", targetNodeName,
		"movablePods", len(movablePods),
		"cpuDeficit", cpuDeficit,
		"memoryDeficit", memoryDeficit)
	
	// Log details about each movable pod for debugging
	for i, movablePod := range movablePods {
		klog.V(4).InfoS("Movable pod details",
			"index", i,
			"pod", klog.KObj(movablePod),
			"currentNode", movablePod.Spec.NodeName,
			"targetNode", targetNodeName,
			"cpuRequest", getPodCPURequest(movablePod))
	}
	
	// Sort pods by CPU request (largest first for better bin-packing)
	sort.Slice(movablePods, func(i, j int) bool {
		return getPodCPURequest(movablePods[i]) > getPodCPURequest(movablePods[j])
	})

	// Try to find optimal combination of moves and evictions
	return pl.optimizeBinPacking(targetNodeName, cpuDeficit, memoryDeficit, movablePods, nodeStates)
}

// optimizeBinPacking uses dynamic programming approach to find optimal bin-packing
func (pl *MyCrossNodePreemptionBinpacking) optimizeBinPacking(
	targetNodeName string,
	cpuDeficit, memoryDeficit int64,
	movablePods []*v1.Pod,
	nodeStates map[string]*NodeResourceState,
) *BinPackingSolution {
	
	// Try different combinations of pod movements
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

	for _, podToMove := range movablePods {
		if len(bestSolution.PodMovements) >= pl.args.MaxMovesPerPod {
			break
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
		if destNode != "" && destNode != targetNodeName {
			klog.V(4).InfoS("Found destination for pod movement",
				"pod", klog.KObj(podToMove),
				"from", podToMove.Spec.NodeName,
				"to", destNode,
				"targetNode", targetNodeName)
			// Can move this pod
			movement := PodMovement{
				Pod:        podToMove,
				FromNode:   podToMove.Spec.NodeName,
				ToNode:     destNode,
				CPURequest: podCPU,
			}
			bestSolution.PodMovements = append(bestSolution.PodMovements, movement)
			
			// Update simulation states
			simStates[targetNodeName].AvailableCPU += podCPU
			simStates[targetNodeName].AvailableMemory += podMemory
			simStates[destNode].AvailableCPU -= podCPU
			simStates[destNode].AvailableMemory -= podMemory
			
			freedCPU += podCPU
			freedMemory += podMemory
			totalMovedCPU += podCPU

			// Check if we've freed enough resources
			if freedCPU >= cpuDeficit && freedMemory >= memoryDeficit {
				break
			}
		} else {
			// Cannot move, must evict
			bestSolution.VictimsToEvict = append(bestSolution.VictimsToEvict, podToMove)
			freedCPU += podCPU
			freedMemory += podMemory

			// Check if we've freed enough resources
			if freedCPU >= cpuDeficit && freedMemory >= memoryDeficit {
				break
			}
		}
	}

	// Check if solution is viable
	if freedCPU < cpuDeficit || freedMemory < memoryDeficit {
		return nil
	}

	// Calculate utilization gain (prefer moves over evictions)
	evictedCPU := int64(0)
	for _, victim := range bestSolution.VictimsToEvict {
		evictedCPU += getPodCPURequest(victim)
	}

	// Utilization gain is better if we move more and evict less
	bestSolution.UtilizationGain = float64(totalMovedCPU) / float64(totalMovedCPU + evictedCPU)
	bestSolution.TotalCPUSaved = evictedCPU

	klog.V(4).InfoS("Bin-packing solution calculated",
		"targetNode", targetNodeName,
		"movements", len(bestSolution.PodMovements),
		"evictions", len(bestSolution.VictimsToEvict),
		"utilizationGain", bestSolution.UtilizationGain,
		"cpuSaved", bestSolution.TotalCPUSaved)

	return bestSolution
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

		// Skip control-plane nodes and nodes with scheduling issues
		if isControlPlaneNode(nodeName) || !isNodeSchedulable(state) {
			continue
		}

		// Check if node can accommodate the pod
		if state.AvailableCPU >= podCPU && state.AvailableMemory >= podMemory {
			klog.V(5).InfoS("Evaluating destination node",
				"node", nodeName,
				"podCPU", podCPU,
				"availableCPU", state.AvailableCPU,
				"wouldFit", state.AvailableCPU >= podCPU)
				
			// Calculate utilization after placing pod
			newCPUUsed := (state.AllocatableCPU - state.AvailableCPU) + podCPU
			newUtilization := float64(newCPUUsed) / float64(state.AllocatableCPU)
			
			// Prefer nodes with better utilization balance
			if newUtilization < bestUtilization {
				bestNode = nodeName
				bestUtilization = newUtilization
			}
		}
	}

	return bestNode
}

// Helper functions
func (pl *MyCrossNodePreemptionBinpacking) getMovablePods(pods []*v1.Pod, minPriority int32) []*v1.Pod {
	var movable []*v1.Pod
	for _, pod := range pods {
		if getPodPriority(pod) < minPriority {
			movable = append(movable, pod)
		}
	}
	return movable
}

func (pl *MyCrossNodePreemptionBinpacking) copyNodeStates(original map[string]*NodeResourceState) map[string]*NodeResourceState {
	copy := make(map[string]*NodeResourceState)
	for name, state := range original {
		copy[name] = &NodeResourceState{
			Name:               state.Name,
			AvailableCPU:       state.AvailableCPU,
			AvailableMemory:    state.AvailableMemory,
			AllocatableCPU:     state.AllocatableCPU,
			AllocatableMemory:  state.AllocatableMemory,
			UtilizationCPU:     state.UtilizationCPU,
		}
	}
	return copy
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
	// Basic check - node should have some available resources
	return state.AllocatableCPU > 0 && state.AllocatableMemory > 0
}
