/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "L	for _, nodeInfo := range viableNodes {
		// Skip nodes that failed on non-preemptible plugins
		nodeName := nodeInfo.Node().Name
		if status, exists := m[nodeName]; exists {
			// Simple check - if node failed for reasons other than resource constraints,
			// it might not be suitable for preemption
			if !status.IsSuccess() {
				continue
			}
		});
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mycrossnodepreemption

import (
	"context"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// findOptimizedCandidates implements the main candidate discovery algorithm.
func (pl *MyCrossNodePreemption) findOptimizedCandidates(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, m framework.NodeToStatusMap, nodeInfos framework.NodeInfoLister) ([]*PreemptionCandidate, *framework.Status) {

	allNodes, err := nodeInfos.List()
	if err != nil {
		return nil, framework.AsStatus(err)
	}

	if len(allNodes) == 0 {
		return nil, framework.NewStatus(framework.Error, "no nodes available")
	}

	// Phase 1: Filter nodes that could potentially fit the pod
	viableNodes := pl.filterViableNodes(ctx, state, pod, allNodes, m)
	pl.logger.V(4).Info("🔎 Filtered viable nodes", 
		"pod", klog.KObj(pod),
		"totalNodes", len(allNodes),
		"viableNodes", len(viableNodes))

	if len(viableNodes) == 0 {
		return nil, framework.NewStatus(framework.Unschedulable, "no viable nodes for preemption")
	}

	// Phase 2: Find preemption candidates using optimized algorithm
	candidates := make([]*PreemptionCandidate, 0)
	
	for _, nodeInfo := range viableNodes {
		select {
		case <-ctx.Done():
			pl.logger.V(2).Info("⏰ Candidate search timed out", "pod", klog.KObj(pod))
			break
		default:
		}

		nodeCandidates := pl.findNodeCandidates(ctx, state, pod, nodeInfo)
		candidates = append(candidates, nodeCandidates...)

		// Limit candidates to prevent excessive computation
		if len(candidates) >= int(pl.args.MaxCandidates) {
			pl.logger.V(4).Info("🛑 Reached maximum candidate limit", 
				"pod", klog.KObj(pod),
				"maxCandidates", pl.args.MaxCandidates)
			break
		}
	}

	// Phase 3: Score and rank candidates
	pl.scoreCandidates(candidates, pod)

	// Phase 4: Sort by score (higher is better)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	return candidates, framework.NewStatus(framework.Success)
}

// filterViableNodes filters nodes that could potentially schedule the pod after preemption.
func (pl *MyCrossNodePreemption) filterViableNodes(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, allNodes []*framework.NodeInfo, m framework.NodeToStatusMap) []*framework.NodeInfo {

	viableNodes := make([]*framework.NodeInfo, 0, len(allNodes))
	
	for _, nodeInfo := range allNodes {
		// Quick resource check - can this node potentially fit the pod?
		if !pl.nodeCouldFitAfterPreemption(pod, nodeInfo) {
			continue
		}

		viableNodes = append(viableNodes, nodeInfo)
	}

	return viableNodes
}

// nodeCouldFitAfterPreemption performs a quick check if the node could fit the pod after preemption.
func (pl *MyCrossNodePreemption) nodeCouldFitAfterPreemption(pod *v1.Pod, nodeInfo *framework.NodeInfo) bool {
	// Check if total node capacity can accommodate the pod
	podRequest := make(v1.ResourceList)
	for _, container := range pod.Spec.Containers {
		for resourceName, quantity := range container.Resources.Requests {
			if existing, ok := podRequest[resourceName]; ok {
				existing.Add(quantity)
				podRequest[resourceName] = existing
			} else {
				podRequest[resourceName] = quantity
			}
		}
	}
	
	nodeCapacity := nodeInfo.Node().Status.Capacity

	for resource, requestedAmount := range podRequest {
		capacityQuantity := nodeCapacity[resource]
		if capacityQuantity.Cmp(requestedAmount) < 0 {
			return false
		}
	}

	return true
}

// findNodeCandidates finds all possible preemption candidates on a specific node.
func (pl *MyCrossNodePreemption) findNodeCandidates(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfo *framework.NodeInfo) []*PreemptionCandidate {

	// Get all pods on the node that can be preempted
	preemptiblePods := pl.getPreemptiblePods(pod, nodeInfo)
	if len(preemptiblePods) == 0 {
		return nil
	}

	pl.logger.V(5).Info("🔍 Analyzing node for preemption", 
		"pod", klog.KObj(pod),
		"node", nodeInfo.Node().Name,
		"preemptiblePods", len(preemptiblePods))

	// Use optimized algorithm based on configuration
	if pl.args.EnableBranchCutting {
		return pl.findCandidatesWithBranchCutting(ctx, state, pod, nodeInfo, preemptiblePods)
	}
	
	return pl.findCandidatesGreedy(ctx, state, pod, nodeInfo, preemptiblePods)
}

// findCandidatesWithBranchCutting uses an optimized DFS with branch cutting.
func (pl *MyCrossNodePreemption) findCandidatesWithBranchCutting(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfo *framework.NodeInfo, preemptiblePods []*v1.Pod) []*PreemptionCandidate {

	// Sort pods by priority (lower priority first for better pruning)
	sort.Slice(preemptiblePods, func(i, j int) bool {
		return getPodPriority(preemptiblePods[i]) < getPodPriority(preemptiblePods[j])
	})

	candidates := make([]*PreemptionCandidate, 0)
	bestScore := float64(-1)
	
	// Use DFS with branch cutting
	pl.dfsWithPruning(ctx, state, pod, nodeInfo, preemptiblePods, 0, []*v1.Pod{}, 
		&candidates, &bestScore)

	return candidates
}

// dfsWithPruning implements depth-first search with intelligent pruning.
func (pl *MyCrossNodePreemption) dfsWithPruning(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfo *framework.NodeInfo, preemptiblePods []*v1.Pod, index int,
	currentVictims []*v1.Pod, candidates *[]*PreemptionCandidate, bestScore *float64) {

	select {
	case <-ctx.Done():
		return
	default:
	}

	// Check if current victims can accommodate the pod
	if pl.canScheduleAfterPreemption(ctx, state, pod, nodeInfo, currentVictims) {
		candidate := &PreemptionCandidate{
			NodeName: nodeInfo.Node().Name,
			Victims:  make([]*v1.Pod, len(currentVictims)),
		}
		copy(candidate.Victims, currentVictims)
		
		// Calculate score
		score := pl.calculateCandidateScore(candidate, pod)
		candidate.Score = score
		
		*candidates = append(*candidates, candidate)
		
		// Update best score for pruning
		if score > *bestScore {
			*bestScore = score
		}
		
		// Early termination if we find a very good candidate
		if score > 0.9 && len(currentVictims) <= 2 {
			return
		}
	}

	// Branch cutting: stop if we have too many victims or poor potential
	if len(currentVictims) > 5 {
		return
	}

	// Estimate upper bound score for remaining candidates
	if pl.estimateUpperBound(preemptiblePods[index:], currentVictims) <= *bestScore {
		return
	}

	// Continue DFS
	for i := index; i < len(preemptiblePods); i++ {
		newVictims := append(currentVictims, preemptiblePods[i])
		pl.dfsWithPruning(ctx, state, pod, nodeInfo, preemptiblePods, i+1, newVictims, candidates, bestScore)
	}
}

// findCandidatesGreedy uses a greedy algorithm for faster candidate discovery.
func (pl *MyCrossNodePreemption) findCandidatesGreedy(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfo *framework.NodeInfo, preemptiblePods []*v1.Pod) []*PreemptionCandidate {

	// Sort by priority and resource usage for greedy selection
	sort.Slice(preemptiblePods, func(i, j int) bool {
		priI, priJ := getPodPriority(preemptiblePods[i]), getPodPriority(preemptiblePods[j])
		if priI != priJ {
			return priI < priJ // Lower priority first
		}
		// If same priority, prefer pods using more resources
		resourceI := pl.calculatePodResourceUsage(preemptiblePods[i])
		resourceJ := pl.calculatePodResourceUsage(preemptiblePods[j])
		return resourceI > resourceJ
	})

	candidates := make([]*PreemptionCandidate, 0)
	victims := make([]*v1.Pod, 0)

	// Greedy selection: add pods one by one until we can schedule
	for _, victim := range preemptiblePods {
		select {
		case <-ctx.Done():
			return candidates
		default:
		}

		victims = append(victims, victim)
		
		if pl.canScheduleAfterPreemption(ctx, state, pod, nodeInfo, victims) {
			candidate := &PreemptionCandidate{
				NodeName: nodeInfo.Node().Name,
				Victims:  make([]*v1.Pod, len(victims)),
			}
			copy(candidate.Victims, victims)
			candidates = append(candidates, candidate)
			
			// Try to find better solutions with fewer victims
			break
		}
	}

	return candidates
}

// getPreemptiblePods returns pods on the node that can be preempted by the given pod.
func (pl *MyCrossNodePreemption) getPreemptiblePods(pod *v1.Pod, nodeInfo *framework.NodeInfo) []*v1.Pod {
	preemptiblePods := make([]*v1.Pod, 0)
	podPriority := getPodPriority(pod)

	for _, p := range nodeInfo.Pods {
		if p.Pod.DeletionTimestamp != nil {
			continue
		}
		
		if getPodPriority(p.Pod) < podPriority {
			preemptiblePods = append(preemptiblePods, p.Pod)
		}
	}

	return preemptiblePods
}

// canScheduleAfterPreemption checks if the pod can be scheduled after removing victims.
func (pl *MyCrossNodePreemption) canScheduleAfterPreemption(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfo *framework.NodeInfo, victims []*v1.Pod) bool {

	// Create a copy of node info and remove victims
	victimsSet := sets.Set[string]{}
	for _, victim := range victims {
		victimsSet.Insert(string(victim.UID))
	}

	// Create a new nodeInfo and remove victims
	nodeInfoCopy := framework.NewNodeInfo()
	nodeInfoCopy.SetNode(nodeInfo.Node())
	
	// Add all pods except victims
	for _, p := range nodeInfo.Pods {
		if !victimsSet.Has(string(p.Pod.UID)) {
			nodeInfoCopy.AddPod(p.Pod)
		}
	}

	// Check if the pod fits
	status := pl.fh.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfoCopy)
	return status.IsSuccess()
}

// estimateUpperBound estimates the maximum possible score for remaining candidates.
func (pl *MyCrossNodePreemption) estimateUpperBound(remainingPods []*v1.Pod, currentVictims []*v1.Pod) float64 {
	if len(remainingPods) == 0 {
		return 0.0
	}
	
	// Simple heuristic: assume we can achieve high score with minimal additional victims
	return 1.0 - float64(len(currentVictims))*0.1
}

// calculatePodResourceUsage calculates the resource usage of a pod.
func (pl *MyCrossNodePreemption) calculatePodResourceUsage(pod *v1.Pod) float64 {
	usage := 0.0
	
	for _, container := range pod.Spec.Containers {
		if cpu, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			usage += float64(cpu.MilliValue()) / 1000.0
		}
		
		if memory, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
			usage += float64(memory.Value()) / (1024 * 1024 * 1024) // Convert to GB
		}
	}
	
	return usage
}
