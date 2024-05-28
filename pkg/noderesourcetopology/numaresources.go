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
	"fmt"
	"reflect"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

type NUMANode struct {
	NUMAID    int
	Resources corev1.ResourceList
	Costs     map[int]int
}

func (n *NUMANode) WithCosts(costs map[int]int) *NUMANode {
	n.Costs = costs
	return n
}

func (n NUMANode) DeepCopy() NUMANode {
	ret := NUMANode{
		NUMAID:    n.NUMAID,
		Resources: n.Resources.DeepCopy(),
	}
	if len(n.Costs) > 0 {
		ret.Costs = make(map[int]int)
		for key, val := range n.Costs {
			ret.Costs[key] = val
		}
	}
	return ret
}

func (n NUMANode) Equal(o NUMANode) bool {
	if n.NUMAID != o.NUMAID {
		return false
	}
	if !reflect.DeepEqual(n.Costs, o.Costs) {
		return false
	}
	return equalResourceList(n.Resources, o.Resources)
}

func equalResourceList(ra, rb corev1.ResourceList) bool {
	if len(ra) != len(rb) {
		return false
	}
	for key, valA := range ra {
		valB, ok := rb[key]
		if !ok {
			return false
		}
		if !valA.Equal(valB) {
			return false
		}
	}
	return true
}

type NUMANodeList []NUMANode

func (nnl NUMANodeList) DeepCopy() NUMANodeList {
	ret := make(NUMANodeList, 0, len(nnl))
	for idx := 0; idx < len(nnl); idx++ {
		ret = append(ret, nnl[idx].DeepCopy())
	}
	return ret
}

func (nnl NUMANodeList) Equal(oth NUMANodeList) bool {
	if len(nnl) != len(oth) {
		return false
	}
	for idx := 0; idx < len(nnl); idx++ {
		if !nnl[idx].Equal(oth[idx]) {
			return false
		}
	}
	return true
}

func isHostLevelResource(resource corev1.ResourceName) bool {
	// host-level resources are resources which *may* not be bound to NUMA nodes.
	// A Key example is generic [ephemeral] storage which doesn't expose NUMA affinity.
	if resource == corev1.ResourceEphemeralStorage {
		return true
	}
	if resource == corev1.ResourceStorage {
		return true
	}
	if !v1helper.IsNativeResource(resource) {
		return true
	}
	return false
}

func isNUMAAffineResource(resource corev1.ResourceName) bool {
	// NUMA-affine resources are resources which are required to be bound to NUMA nodes.
	// A Key example is CPU and memory, which must expose NUMA affinity.
	if resource == corev1.ResourceCPU {
		return true
	}
	if resource == corev1.ResourceMemory {
		return true
	}
	if v1helper.IsHugePageResourceName(resource) {
		return true
	}
	// Devices are *expected* to expose NUMA Affinity, but they are not *required* to do so.
	// We can't tell for sure, so we default to "no".
	return false
}

func isResourceSetSuitable(qos corev1.PodQOSClass, resource corev1.ResourceName, quantity, numaQuantity resource.Quantity) bool {
	if qos != corev1.PodQOSGuaranteed && isNUMAAffineResource(resource) {
		return true
	}
	return numaQuantity.Cmp(quantity) >= 0
}

// subtractResourcesFromNUMANodeList finds the correct NUMA ID's resources and always subtract them from `nodes` in-place.
func subtractResourcesFromNUMANodeList(lh logr.Logger, nodes NUMANodeList, numaID int, qos corev1.PodQOSClass, containerRes corev1.ResourceList) error {
	logEntries := []any{"numaCell", numaID}

	for _, node := range nodes {
		if node.NUMAID != numaID {
			continue
		}

		lh.V(5).Info("NUMA resources before", append(logEntries, stringify.ResourceListToLoggable(node.Resources)...)...)

		for resName, resQty := range containerRes {
			isAffine := isNUMAAffineResource(resName)
			if qos != corev1.PodQOSGuaranteed && isAffine {
				lh.V(4).Info("ignoring QoS-depending exclusive request", "resource", resName, "QoS", qos)
				continue
			}
			if resQty.IsZero() {
				lh.V(4).Info("ignoring zero-valued request", "resource", resName)
				continue
			}
			nResQ, ok := node.Resources[resName]
			if !ok {
				lh.V(4).Info("ignoring missing resource", "resource", resName, "affine", isAffine, "request", resQty.String())
				continue
			}
			nodeResQty := nResQ.DeepCopy()
			nodeResQty.Sub(resQty)
			if nodeResQty.Sign() < 0 {
				lh.V(1).Info("resource quantity should not be a negative value", "numaCell", numaID, "resource", resName, "quantity", nResQ.String(), "request", resQty.String())
				return fmt.Errorf("resource %q request %s exceeds NUMA %d availability %s", string(resName), resQty.String(), numaID, nResQ.String())
			}
			node.Resources[resName] = nodeResQty
		}

		lh.V(5).Info("NUMA resources after", append(logEntries, stringify.ResourceListToLoggable(node.Resources)...)...)
	}
	return nil
}

func subtractFromNUMAs(resources corev1.ResourceList, numaNodes NUMANodeList, nodes ...int) {
	for resName, quantity := range resources {
		for _, node := range nodes {
			// quantity is zero no need to iterate through another NUMA node, go to another resource
			if quantity.IsZero() {
				break
			}

			nRes := numaNodes[node].Resources
			if available, ok := nRes[resName]; ok {
				switch quantity.Cmp(available) {
				case 0: // the same
					// basically zero container resources
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case 1: // container wants more resources than available in this NUMA zone
					// substract NUMA resources from container request, to calculate how much is missing
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case -1: // there are more resources available in this NUMA zone than container requests
					// substract container resources from resources available in this NUMA node
					available.Sub(quantity)
					// zero container quantity
					quantity = resource.Quantity{}
					nRes[resName] = available
				}
			}
		}
	}
}
