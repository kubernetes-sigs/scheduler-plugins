/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
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
	"math"
	"sort"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	policylisters "k8s.io/client-go/listers/policy/v1"
	"k8s.io/klog/v2"
)

// getPodPriority returns the priority of a pod.
func getPodPriority(pod *v1.Pod) int32 {
	if pod.Spec.Priority != nil {
		return *pod.Spec.Priority
	}
	return 0
}

// scoreCandidates assigns scores to all candidates based on multiple criteria.
func (pl *MyCrossNodePreemption) scoreCandidates(candidates []*PreemptionCandidate, pod *v1.Pod) {
	if len(candidates) == 0 {
		return
	}

	pl.logger.V(4).Info("📊 Scoring preemption candidates", 
		"pod", klog.KObj(pod),
		"candidateCount", len(candidates))

	for _, candidate := range candidates {
		candidate.Score = pl.calculateCandidateScore(candidate, pod)
		candidate.ResourceCost = pl.calculateResourceCost(candidate)
		candidate.Priority = pl.calculateAveragePriority(candidate)
	}
}

// calculateCandidateScore computes a comprehensive score for a preemption candidate.
func (pl *MyCrossNodePreemption) calculateCandidateScore(candidate *PreemptionCandidate, pod *v1.Pod) float64 {
	// Base score components
	priorityScore := pl.calculatePriorityScore(candidate, pod)
	resourceScore := pl.calculateResourceScore(candidate, pod)
	topologyScore := pl.calculateTopologyScore(candidate, pod)
	pdbScore := pl.calculatePDBScore(candidate)

	// Weighted combination
	weights := pl.args.ScoreWeights
	totalScore := priorityScore*weights.Priority + 
		resourceScore*weights.Resources + 
		topologyScore*weights.Topology

	// Apply PDB penalty
	totalScore *= pdbScore

	// Normalize to [0, 1] range
	return math.Max(0.0, math.Min(1.0, totalScore))
}

// calculatePriorityScore scores based on victim priorities vs preemptor priority.
func (pl *MyCrossNodePreemption) calculatePriorityScore(candidate *PreemptionCandidate, pod *v1.Pod) float64 {
	if len(candidate.Victims) == 0 {
		return 0.0
	}

	preemptorPriority := getPodPriority(pod)
	totalPriorityDiff := int32(0)
	
	for _, victim := range candidate.Victims {
		victimPriority := getPodPriority(victim)
		priorityDiff := preemptorPriority - victimPriority
		
		// Only consider if preemptor has higher priority
		if priorityDiff > 0 {
			totalPriorityDiff += priorityDiff
		}
	}

	// Normalize by number of victims and maximum possible priority difference
	avgPriorityDiff := float64(totalPriorityDiff) / float64(len(candidate.Victims))
	maxPriorityDiff := 1000.0 // Assume max priority difference is 1000
	
	return math.Min(1.0, avgPriorityDiff/maxPriorityDiff)
}

// calculateResourceScore scores based on resource efficiency.
func (pl *MyCrossNodePreemption) calculateResourceScore(candidate *PreemptionCandidate, pod *v1.Pod) float64 {
	if len(candidate.Victims) == 0 {
		return 0.0
	}

	// Calculate resources requested by the preemptor
	preemptorResources := pl.calculatePodResourceUsage(pod)
	
	// Calculate total resources freed by victims
	freedResources := 0.0
	for _, victim := range candidate.Victims {
		freedResources += pl.calculatePodResourceUsage(victim)
	}

	// Resource efficiency: how much of freed resources will be used
	efficiency := preemptorResources / math.Max(freedResources, 0.001)
	
	// Prefer candidates that free just enough resources (minimize waste)
	if efficiency > 1.0 {
		return 0.0 // Not enough resources freed
	}
	
	// Score higher for better resource utilization
	wasteScore := 1.0 - math.Abs(1.0-efficiency)
	
	// Penalize having too many victims
	victimPenalty := 1.0 / (1.0 + float64(len(candidate.Victims))*0.1)
	
	return wasteScore * victimPenalty
}

// calculateTopologyScore scores based on pod topology and affinity.
func (pl *MyCrossNodePreemption) calculateTopologyScore(candidate *PreemptionCandidate, pod *v1.Pod) float64 {
	// Simple topology scoring based on node labels and pod affinity
	score := 0.5 // Base score
	
	// Check if the pod has node affinity preferences
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
		if pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
			// Assume node matches some preferences (simplified)
			score += 0.3
		}
	}
	
	// Check anti-affinity considerations
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil {
		// Penalize if we're breaking anti-affinity by placing here
		score -= 0.2
	}
	
	return math.Max(0.0, math.Min(1.0, score))
}

// calculatePDBScore checks PodDisruptionBudget violations.
func (pl *MyCrossNodePreemption) calculatePDBScore(candidate *PreemptionCandidate) float64 {
	if !pl.args.ConsiderPDBs {
		return 1.0 // No PDB consideration
	}

	pdbLister := pl.getPDBLister()
	if pdbLister == nil {
		return 1.0 // No PDB lister available
	}

	score := 1.0
	
	for _, victim := range candidate.Victims {
		if pl.wouldViolatePDB(victim, pdbLister) {
			score *= 0.5 // Penalize PDB violations
		}
	}
	
	return score
}

// wouldViolatePDB checks if preempting a pod would violate its PDB.
func (pl *MyCrossNodePreemption) wouldViolatePDB(pod *v1.Pod, pdbLister policylisters.PodDisruptionBudgetLister) bool {
	pdbs, err := pdbLister.PodDisruptionBudgets(pod.Namespace).List(labels.Everything())
	if err != nil {
		pl.logger.V(4).Info("Failed to list PDBs", "error", err)
		return false
	}

	for _, pdb := range pdbs {
		if pl.podMatchesPDB(pod, pdb) {
			// Simplified PDB check - in real implementation, we'd need to 
			// count current healthy pods and check against min available
			pl.logger.V(5).Info("Pod matches PDB", 
				"pod", klog.KObj(pod), 
				"pdb", klog.KObj(pdb))
			return true // Conservative approach
		}
	}
	
	return false
}

// podMatchesPDB checks if a pod is covered by a PDB.
func (pl *MyCrossNodePreemption) podMatchesPDB(pod *v1.Pod, pdb *policy.PodDisruptionBudget) bool {
	if pdb.Namespace != pod.Namespace {
		return false
	}
	
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return false
	}
	
	return selector.Matches(labels.Set(pod.Labels))
}

// calculateResourceCost computes the total resource cost of preempting victims.
func (pl *MyCrossNodePreemption) calculateResourceCost(candidate *PreemptionCandidate) float64 {
	totalCost := 0.0
	
	for _, victim := range candidate.Victims {
		// Cost includes CPU, memory, and other resources
		cost := pl.calculatePodResourceUsage(victim)
		
		// Add priority-based cost multiplier
		priority := getPodPriority(victim)
		priorityMultiplier := 1.0 + float64(priority)/1000.0
		
		totalCost += cost * priorityMultiplier
	}
	
	return totalCost
}

// calculateAveragePriority computes the average priority of victim pods.
func (pl *MyCrossNodePreemption) calculateAveragePriority(candidate *PreemptionCandidate) int32 {
	if len(candidate.Victims) == 0 {
		return 0
	}
	
	totalPriority := int32(0)
	for _, victim := range candidate.Victims {
		totalPriority += getPodPriority(victim)
	}
	
	return totalPriority / int32(len(candidate.Victims))
}

// selectBestCandidate implements intelligent candidate selection with multiple criteria.
func (pl *MyCrossNodePreemption) selectBestCandidate(candidates []*PreemptionCandidate, pod *v1.Pod) *PreemptionCandidate {
	if len(candidates) == 0 {
		return nil
	}

	pl.logger.V(4).Info("🎯 Selecting best candidate", 
		"pod", klog.KObj(pod),
		"totalCandidates", len(candidates))

	// Primary sort: by score (descending)
	sort.Slice(candidates, func(i, j int) bool {
		if math.Abs(candidates[i].Score-candidates[j].Score) > 0.01 {
			return candidates[i].Score > candidates[j].Score
		}
		
		// Secondary sort: by number of victims (ascending - fewer is better)
		if len(candidates[i].Victims) != len(candidates[j].Victims) {
			return len(candidates[i].Victims) < len(candidates[j].Victims)
		}
		
		// Tertiary sort: by resource cost (ascending - lower cost is better)
		return candidates[i].ResourceCost < candidates[j].ResourceCost
	})

	bestCandidate := candidates[0]
	
	pl.logger.Info("🏆 Best candidate selected", 
		"pod", klog.KObj(pod),
		"node", bestCandidate.NodeName,
		"score", bestCandidate.Score,
		"victims", len(bestCandidate.Victims),
		"resourceCost", bestCandidate.ResourceCost,
		"avgPriority", bestCandidate.Priority)

	// Log top candidates for debugging
	for i, candidate := range candidates {
		if i >= 3 { // Log top 3 candidates
			break
		}
		pl.logger.V(5).Info("📋 Candidate details", 
			"rank", i+1,
			"node", candidate.NodeName,
			"score", candidate.Score,
			"victims", len(candidate.Victims),
			"resourceCost", candidate.ResourceCost)
	}

	return bestCandidate
}

// getPDBLister gets the PDB lister if available.
func (pl *MyCrossNodePreemption) getPDBLister() policylisters.PodDisruptionBudgetLister {
	if pl.fh.SharedInformerFactory() == nil {
		return nil
	}
	
	// Try to get PDB lister - this might not be available in all configurations
	informer := pl.fh.SharedInformerFactory().Policy().V1().PodDisruptionBudgets()
	if informer == nil {
		return nil
	}
	
	return informer.Lister()
}

// Helper structures for compatibility with default preemption

type defaultCandidate struct {
	victims []*v1.Pod
	name    string
}

func (c *defaultCandidate) Victims() []*v1.Pod {
	return c.victims
}

func (c *defaultCandidate) Name() string {
	return c.name
}

// convertToDefaultCandidates converts our candidates to a simpler format.
func (pl *MyCrossNodePreemption) convertToDefaultCandidates(candidates []*PreemptionCandidate) []*defaultCandidate {
	dpCandidates := make([]*defaultCandidate, len(candidates))
	
	for i, candidate := range candidates {
		dpCandidates[i] = &defaultCandidate{
			victims: candidate.Victims,
			name:    candidate.NodeName,
		}
	}
	
	return dpCandidates
}
