/*
Copyright 2021 The Kubernetes Authors.

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

package noderesourcetopology

import (
	"context"
	"fmt"

	"gonum.org/v1/gonum/stat"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

const (
	defaultWeight = int64(1)
)

type scoreStrategyFn func(v1.ResourceList, v1.ResourceList, resourceToWeightMap) int64

// resourceToWeightMap contains resource name and weight.
type resourceToWeightMap map[v1.ResourceName]int64

// weight return the weight of the resource and defaultWeight if weight not specified
func (rw resourceToWeightMap) weight(r v1.ResourceName) int64 {
	w, ok := (rw)[r]
	if !ok {
		return defaultWeight
	}

	if w < 1 {
		return defaultWeight
	}

	return w
}

func (tm *TopologyMatch) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	klog.V(6).InfoS("scoring node", "nodeName", nodeName)
	// if it's a non-guaranteed pod, every node is considered to be a good fit
	if v1qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
		return framework.MaxNodeScore, nil
	}

	nodeTopology, ok := tm.nrtCache.GetCachedNRTCopy(ctx, nodeName, pod)

	if !ok {
		klog.V(4).InfoS("noderesourcetopology is not valid for node", "node", nodeName)
		return 0, nil
	}
	if nodeTopology == nil {
		klog.V(5).InfoS("noderesourcetopology was not found for node", "node", nodeName)
		return 0, nil
	}

	logNRT("noderesourcetopology found", nodeTopology)

	handler := tm.scoringHandlerFromTopologyManagerConfig(topologyManagerConfigFromNodeResourceTopology(nodeTopology))
	if handler == nil {
		return 0, nil
	}
	return handler(pod, nodeTopology.Zones)
}

func (tm *TopologyMatch) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// scoreForEachNUMANode will iterate over all NUMA zones of the node and invoke the scoreStrategyFn func for every zone.
// it will return the minimal score of all the calculated NUMA's score, in order to avoid edge cases.
func scoreForEachNUMANode(requested v1.ResourceList, numaList NUMANodeList, score scoreStrategyFn, resourceToWeightMap resourceToWeightMap) int64 {
	numaScores := make([]int64, len(numaList))
	minScore := int64(0)

	for _, numa := range numaList {
		numaScore := score(requested, numa.Resources, resourceToWeightMap)
		// if NUMA's score is 0, i.e. not fit at all, it won't be taken under consideration by Kubelet.
		if (minScore == 0) || (numaScore != 0 && numaScore < minScore) {
			minScore = numaScore
		}
		numaScores[numa.NUMAID] = numaScore
		klog.V(6).InfoS("numa score result", "numaID", numa.NUMAID, "score", numaScore)
	}
	return minScore
}

func getScoringStrategyFunction(strategy apiconfig.ScoringStrategyType) (scoreStrategyFn, error) {
	switch strategy {
	case apiconfig.MostAllocated:
		return mostAllocatedScoreStrategy, nil
	case apiconfig.LeastAllocated:
		return leastAllocatedScoreStrategy, nil
	case apiconfig.BalancedAllocation:
		return balancedAllocationScoreStrategy, nil
	case apiconfig.LeastNUMANodes:
		// this is a special case handled down the flow. We just need to NOT error out.
		return nil, nil
	default:
		return nil, fmt.Errorf("illegal scoring strategy found")
	}
}

func podScopeScore(pod *v1.Pod, zones topologyv1alpha2.ZoneList, scorerFn scoreStrategyFn, resourceToWeightMap resourceToWeightMap) (int64, *framework.Status) {
	// This code is in Admit implementation of pod scope
	// https://github.com/kubernetes/kubernetes/blob/9ff3b7e744b34c099c1405d9add192adbef0b6b1/pkg/kubelet/cm/topologymanager/scope_pod.go#L52
	// but it works with HintProviders, takes into account all possible allocations.
	resources := util.GetPodEffectiveRequest(pod)

	allocatablePerNUMA := createNUMANodeList(zones)
	finalScore := scoreForEachNUMANode(resources, allocatablePerNUMA, scorerFn, resourceToWeightMap)
	klog.V(5).InfoS("pod scope scoring final node score", "finalScore", finalScore)
	return finalScore, nil
}

func containerScopeScore(pod *v1.Pod, zones topologyv1alpha2.ZoneList, scorerFn scoreStrategyFn, resourceToWeightMap resourceToWeightMap) (int64, *framework.Status) {
	// This code is in Admit implementation of container scope
	// https://github.com/kubernetes/kubernetes/blob/9ff3b7e744b34c099c1405d9add192adbef0b6b1/pkg/kubelet/cm/topologymanager/scope_container.go#L52
	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	contScore := make([]float64, len(containers))
	allocatablePerNUMA := createNUMANodeList(zones)

	for i, container := range containers {
		identifier := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
		contScore[i] = float64(scoreForEachNUMANode(container.Resources.Requests, allocatablePerNUMA, scorerFn, resourceToWeightMap))
		klog.V(6).InfoS("container scope scoring", "container", identifier, "score", contScore[i])
	}
	finalScore := int64(stat.Mean(contScore, nil))
	klog.V(5).InfoS("container scope scoring final node score", "finalScore", finalScore)
	return finalScore, nil
}

func (tm *TopologyMatch) scoringHandlerFromTopologyManagerConfig(conf TopologyManagerConfig) scoringFn {
	if tm.scoreStrategyType == apiconfig.LeastNUMANodes {
		if conf.Scope == kubeletconfig.PodTopologyManagerScope {
			return leastNUMAPodScopeScore
		}
		if conf.Scope == kubeletconfig.ContainerTopologyManagerScope {
			return leastNUMAContainerScopeScore
		}
		return nil // cannot happen
	}
	if conf.Policy != kubeletconfig.SingleNumaNodeTopologyManagerPolicy {
		return nil
	}
	if conf.Scope == kubeletconfig.PodTopologyManagerScope {
		return func(pod *v1.Pod, zones topologyv1alpha2.ZoneList) (int64, *framework.Status) {
			return podScopeScore(pod, zones, tm.scoreStrategyFunc, tm.resourceToWeightMap)
		}
	}
	if conf.Scope == kubeletconfig.ContainerTopologyManagerScope {
		return func(pod *v1.Pod, zones topologyv1alpha2.ZoneList) (int64, *framework.Status) {
			return containerScopeScore(pod, zones, tm.scoreStrategyFunc, tm.resourceToWeightMap)
		}
	}
	return nil // cannot happen
}
