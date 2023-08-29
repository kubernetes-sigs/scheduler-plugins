/*
Copyright 2023 The Kubernetes Authors.

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
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"gonum.org/v1/gonum/stat/combin"

	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

const (
	// 255 is max value as defined by ACPI SLIT(System Locality Information Tables), which means unknown/undefined
	maxDistanceValue = 255
)

func leastNUMAContainerScopeScore(pod *v1.Pod, zones topologyv1alpha2.ZoneList) (int64, *framework.Status) {
	nodes := createNUMANodeList(zones)
	qos := v1qos.GetPodQOS(pod)

	maxNUMANodesCount := 0
	allContainersMinAvgDistance := true
	// the order how TopologyManager asks for hint is important so doing it in the same order
	// https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/cm/topologymanager/scope_container.go#L52
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		// if a container requests only non NUMA just continue
		if onlyNonNUMAResources(nodes, container.Resources.Requests) {
			continue
		}
		identifier := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
		numaNodes, isMinAvgDistance := numaNodesRequired(identifier, qos, nodes, container.Resources.Requests)
		// container's resources can't fit onto node, return MinNodeScore for whole pod
		if numaNodes == nil {
			// score plugin should be running after resource filter plugin so we should always find sufficient amount of NUMA nodes
			klog.Warningf("cannot calculate how many NUMA nodes are required for: %s", identifier)
			return framework.MinNodeScore, nil
		}

		if !isMinAvgDistance {
			allContainersMinAvgDistance = false
		}

		if numaNodes.Count() > maxNUMANodesCount {
			maxNUMANodesCount = numaNodes.Count()
		}

		// subtract the resources requested by the container from the given NUMA.
		// this is necessary, so we won't allocate the same resources for the upcoming containers
		subtractFromNUMAs(container.Resources.Requests, nodes, numaNodes.GetBits()...)
	}

	if maxNUMANodesCount == 0 {
		return framework.MaxNodeScore, nil
	}

	return normalizeScore(maxNUMANodesCount, allContainersMinAvgDistance), nil
}

func leastNUMAPodScopeScore(pod *v1.Pod, zones topologyv1alpha2.ZoneList) (int64, *framework.Status) {
	nodes := createNUMANodeList(zones)
	qos := v1qos.GetPodQOS(pod)

	identifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	resources := util.GetPodEffectiveRequest(pod)
	// if a pod requests only non NUMA resources return max score
	if onlyNonNUMAResources(nodes, resources) {
		return framework.MaxNodeScore, nil
	}

	numaNodes, isMinAvgDistance := numaNodesRequired(identifier, qos, nodes, resources)
	// pod's resources can't fit onto node, return MinNodeScore
	if numaNodes == nil {
		// score plugin should be running after resource filter plugin so we should always find sufficient amount of NUMA nodes
		klog.Warningf("cannot calculate how many NUMA nodes are required for: %s", identifier)
		return framework.MinNodeScore, nil
	}

	return normalizeScore(numaNodes.Count(), isMinAvgDistance), nil
}

func normalizeScore(numaNodesCount int, isMinAvgDistance bool) int64 {
	numaNodeScore := framework.MaxNodeScore / highestNUMAID
	score := framework.MaxNodeScore - int64(numaNodesCount)*numaNodeScore
	if isMinAvgDistance {
		// if distance between NUMA domains is optimal add half of numaNodeScore to make this node more favorable
		return score + numaNodeScore/2
	}

	return score
}

func minAvgDistanceInCombinations(numaNodes NUMANodeList, numaNodesCombination [][]int) float32 {
	// max distance for NUMA node
	var minDistance float32 = maxDistanceValue

	for _, combination := range numaNodesCombination {
		avgDistance := nodesAvgDistance(numaNodes, combination...)
		if avgDistance < minDistance {
			minDistance = avgDistance
		}
	}

	return minDistance
}

func nodesAvgDistance(numaNodes NUMANodeList, nodes ...int) float32 {
	if len(nodes) == 0 {
		return maxDistanceValue
	}

	var (
		accu int
	)

	for _, node1 := range nodes {
		for _, node2 := range nodes {
			cost, ok := numaNodes[node1].Costs[numaNodes[node2].NUMAID]
			// we couldn't read Costs assign maxDistanceValue
			if !ok {
				klog.Warningf("cannot retrieve Costs information for node ID %d", numaNodes[node1].NUMAID)
				cost = maxDistanceValue
			}
			accu += cost
		}
	}

	return float32(accu) / float32(len(nodes)*len(nodes))
}

func combineResources(numaNodes NUMANodeList, combination []int) v1.ResourceList {
	resources := v1.ResourceList{}
	for _, nodeIndex := range combination {
		for resource, quantity := range numaNodes[nodeIndex].Resources {
			if value, ok := resources[resource]; ok {
				value.Add(quantity)
				resources[resource] = value
				continue
			}
			resources[resource] = quantity
		}
	}

	return resources
}

// numaNodesRequired returns bitmask with minimal NUMA nodes required to run given resources
// or nil when resources can't be fitted onto the worker node
// second value returned is a boolean indicating if bitmask is optimal from distance perspective
func numaNodesRequired(identifier string, qos v1.PodQOSClass, numaNodes NUMANodeList, resources v1.ResourceList) (bitmask.BitMask, bool) {
	for bitmaskLen := 1; bitmaskLen <= len(numaNodes); bitmaskLen++ {
		numaNodesCombination := combin.Combinations(len(numaNodes), bitmaskLen)
		suitableCombination, isMinDistance := findSuitableCombination(identifier, qos, numaNodes, resources, numaNodesCombination)
		// we have found suitable combination for given bitmaskLen
		if suitableCombination != nil {
			bm := bitmask.NewEmptyBitMask()
			for _, nodeIdx := range suitableCombination {
				bm.Add(numaNodes[nodeIdx].NUMAID)
			}
			return bm, isMinDistance
		}
	}

	return nil, false
}

// findSuitableCombination returns combination from numaNodesCombination that can fit resources, otherwise return nil
// second value returned is a boolean indicating if returned combination is optimal from distance perspective
// this function will always return combination that provides minimal average distance between nodes in combination
func findSuitableCombination(identifier string, qos v1.PodQOSClass, numaNodes NUMANodeList, resources v1.ResourceList, numaNodesCombination [][]int) ([]int, bool) {
	minAvgDistance := minAvgDistanceInCombinations(numaNodes, numaNodesCombination)
	var (
		minDistanceCombination []int
		// init as max distance
		minDistance float32 = 256
	)
	for _, combination := range numaNodesCombination {
		if !isValidCombineResources(numaNodes, resources, combination) {
			continue
		}
		combinationResources := combineResources(numaNodes, combination)
		resourcesFit := checkResourcesFit(identifier, qos, resources, combinationResources)

		if resourcesFit {
			distance := nodesAvgDistance(numaNodes, combination...)
			if distance == minAvgDistance {
				// return early if we can fit resources into combination and provide minDistance
				return combination, true
			}
			// we don't have to check which combination bitmask has lower value since we are generating them from lowest value
			if distance < minDistance {
				minDistance = distance
				minDistanceCombination = combination
			}
		}
	}

	return minDistanceCombination, false
}

func checkResourcesFit(identifier string, qos v1.PodQOSClass, resources v1.ResourceList, combinationResources v1.ResourceList) bool {
	for resource, quantity := range resources {
		if quantity.IsZero() {
			klog.V(4).InfoS("ignoring zero-qty resource request", "identifier", identifier, "resource", resource)
			continue
		}
		if combinationQuantity := combinationResources[resource]; !isResourceSetSuitable(qos, resource, quantity, combinationQuantity) {
			return false
		}
	}

	return true
}

func isValidCombineResources(numaNodes NUMANodeList, resources v1.ResourceList, combination []int) bool {
	for _, nodeIndex := range combination {
		for resourceName := range resources {
			if _, ok := numaNodes[nodeIndex].Resources[resourceName]; !ok {
				return false
			}
		}
	}
	return true
}
