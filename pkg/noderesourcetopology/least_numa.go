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

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"gonum.org/v1/gonum/stat/combin"

	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

func leastNUMAContainerScopeScore(pod *v1.Pod, zones topologyv1alpha1.ZoneList) (int64, *framework.Status) {
	nodes := createNUMANodeList(zones)
	qos := v1qos.GetPodQOS(pod)

	maxNUMANodesCount := 0
	// the order how TopologyManager asks for hint is important so doing it in the same order
	// https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/cm/topologymanager/scope_container.go#L52
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		identifier := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
		numaNodes := numaNodesRequired(identifier, qos, nodes, container.Resources.Requests)
		// container's resources can't fit onto node, return MinNodeScore for whole pod
		if numaNodes == nil {
			return framework.MinNodeScore, nil
		}

		if numaNodes.Count() > maxNUMANodesCount {
			maxNUMANodesCount = numaNodes.Count()
		}

		// subtract the resources requested by the container from the given NUMA.
		// this is necessary, so we won't allocate the same resources for the upcoming containers
		subtractFromNUMAs(container.Resources.Requests, nodes, numaNodes.GetBits()...)
	}

	return normalizeScore(maxNUMANodesCount), nil
}

func leastNUMAPodScopeScore(pod *v1.Pod, zones topologyv1alpha1.ZoneList) (int64, *framework.Status) {
	nodes := createNUMANodeList(zones)
	qos := v1qos.GetPodQOS(pod)

	identifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	resources := util.GetPodEffectiveRequest(pod)

	numaNodes := numaNodesRequired(identifier, qos, nodes, resources)
	// pod's resources can't fit onto node, return MinNodeScore
	if numaNodes == nil {
		return framework.MinNodeScore, nil
	}

	return normalizeScore(numaNodes.Count()), nil
}

func normalizeScore(numaNodes int) int64 {
	numaScore := framework.MaxNodeScore / highestNUMAID
	return framework.MaxNodeScore - int64(numaNodes)*numaScore
}

// numaNodesRequired returns bitmask with minimal NUMA nodes required to run given resources
// or nil when resources can't be fitted onto the node
func numaNodesRequired(identifier string, qos v1.PodQOSClass, numaNodes NUMANodeList, resources v1.ResourceList) bitmask.BitMask {
	combinationBitmask := bitmask.NewEmptyBitMask()
	// we will generate combination of numa nodes from len = 1 to the number of numa nodes present on the machine
	for i := 1; i <= len(numaNodes); i++ {
		// generate combinations of len i
		numaNodesCombination := combin.Combinations(len(numaNodes), i)
		// iterate over combinations for given i
		for _, combination := range numaNodesCombination {
			// accumulate resources for given combination
			combinationResources := combineResources(numaNodes, combination)

			resourcesFit := true
			onlyNonNUMAResources := true
			for resource, quantity := range resources {
				if quantity.IsZero() {
					// why bother? everything's fine from the perspective of this resource
					klog.V(4).InfoS("ignoring zero-qty resource request", "identifier", identifier, "resource", resource)
					continue
				}

				combinationQuantity, ok := combinationResources[resource]
				if !ok {
					// non NUMA resource continue
					continue
				}

				// there can be a situation where container/pod requests only non NUMA resources
				onlyNonNUMAResources = false

				if !isResourceSetSuitable(qos, resource, quantity, combinationQuantity) {
					resourcesFit = false
					break
				}

			}
			// if resources can be fit on given combination, just return the number of numa nodes requires to fit them
			// according to TopologyManager if both masks are the same size pick the one that has less bits set
			// https://github.com/kubernetes/kubernetes/blob/3e26e104bdf9d0dc3c4046d6350b93557c67f3f4/pkg/kubelet/cm/topologymanager/bitmask/bitmask.go#L146
			// combin.Combinations is generating combinations in an order from the smallest to highest value
			if resourcesFit {
				// if a container/pod requests only non NUMA resources return empty bitmask and score of 0
				if onlyNonNUMAResources {
					return combinationBitmask
				}

				combinationBitmask.Add(combination...)
				return combinationBitmask
			}
		}
	}

	// score plugin should be running after resource filter plugin so we should always find sufficient amount of NUMA nodes
	klog.Warningf("cannot calculate how many NUMA nodes are required for: %s", identifier)
	return nil
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
