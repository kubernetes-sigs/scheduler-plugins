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
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	bm "k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

type PolicyHandler func(pod *v1.Pod, zoneMap topologyv1alpha1.ZoneList) *framework.Status

func singleNUMAContainerLevelHandler(pod *v1.Pod, zones topologyv1alpha1.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status {
	klog.V(5).InfoS("Single NUMA node handler")

	// prepare NUMANodes list from zoneMap
	nodes := createNUMANodeList(zones)
	qos := v1qos.GetPodQOS(pod)

	// Node() != nil already verified in Filter(), which is the only public entry point
	logNumaNodes("container handler NUMA resources", nodeInfo.Node().Name, nodes)

	// We count here in the way TopologyManager is doing it, IOW we put InitContainers
	// and normal containers in the one scope
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		logKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
		klog.V(6).InfoS("target resources", resourceListToLoggable(logKey, container.Resources.Requests)...)

		if !resourcesAvailableInAnyNUMANodes(logKey, nodes, container.Resources.Requests, qos, nodeInfo) {
			// definitely we can't align container, so we can't align a pod
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("cannot align container: %s", container.Name))
		}
	}
	return nil
}

// resourcesAvailableInAnyNUMANodes checks for sufficient resource, this function
// requires NUMANodeList with properly populated NUMANode, NUMAID should be in range 0-63
func resourcesAvailableInAnyNUMANodes(logKey string, numaNodes NUMANodeList, resources v1.ResourceList, qos v1.PodQOSClass, nodeInfo *framework.NodeInfo) bool {
	bitmask := bm.NewEmptyBitMask()
	// set all bits, each bit is a NUMA node, if resources couldn't be aligned
	// on the NUMA node, bit should be unset
	bitmask.Fill()

	// Node() != nil already verified in Filter(), which is the only public entry point
	nodeName := nodeInfo.Node().Name

	for resource, quantity := range resources {
		// for each requested resource, calculate which NUMA slots are good fits, and then AND with the aggregated bitmask, IOW unset appropriate bit if we can't align resources, or set it
		// obvious, bits which are not in the NUMA id's range would be unset
		resourceBitmask := bm.NewEmptyBitMask()
		for _, numaNode := range numaNodes {
			numaQuantity, ok := numaNode.Resources[resource]
			// if the requested resource can't be found on the NUMA node, we still need to check
			// if the resource can be found at the node itself, because there are resources which are not NUMA aligned
			// or not supported by the topology exporter - if resource was not found at both checks - skip (don't set it as available NUMA node).
			// if the un-found resource has 0 quantity probably this numa node can be considered.
			if !ok && !resourceFoundOnNode(resource, quantity, nodeInfo) && !quantity.IsZero() {
				continue
			}

			if !isNUMANodeSuitable(qos, resource, quantity, numaQuantity) {
				continue
			}

			resourceBitmask.Add(numaNode.NUMAID)
			klog.V(6).InfoS("feasible", "logKey", logKey, "node", nodeName, "NUMA", numaNode.NUMAID, "resource", resource)
		}
		bitmask.And(resourceBitmask)
		if bitmask.IsEmpty() {
			klog.V(5).InfoS("early verdict", "logKey", logKey, "node", nodeName, "resource", resource, "suitable", "false")
			return false
		}
	}
	ret := !bitmask.IsEmpty()
	klog.V(5).InfoS("final verdict", "logKey", logKey, "node", nodeName, "suitable", ret)
	return ret
}

func isNUMANodeSuitable(qos v1.PodQOSClass, resource v1.ResourceName, quantity, numaQuantity resource.Quantity) bool {
	// Check for the following:
	if qos != v1.PodQOSGuaranteed {
		// 1. set numa node as possible node if resource is memory or Hugepages
		if resource == v1.ResourceMemory {
			return true
		}
		if strings.HasPrefix(string(resource), v1.ResourceHugePagesPrefix) {
			return true
		}
		// 2. set numa node as possible node if resource is CPU
		if resource == v1.ResourceCPU {
			return true
		}
	}
	// 3. set numa node as possible node if zero quantity for non existing resource was requested
	if quantity.IsZero() {
		return true
	}
	// 4. otherwise check amount of resources
	return numaQuantity.Cmp(quantity) >= 0
}

func singleNUMAPodLevelHandler(pod *v1.Pod, zones topologyv1alpha1.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status {
	klog.V(5).InfoS("Pod Level Resource handler")

	resources := util.GetPodEffectiveRequest(pod)

	logKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	nodes := createNUMANodeList(zones)

	// Node() != nil already verified in Filter(), which is the only public entry point
	logNumaNodes("pod handler NUMA resources", nodeInfo.Node().Name, nodes)
	klog.V(6).InfoS("target resources", resourceListToLoggable(logKey, resources)...)

	if !resourcesAvailableInAnyNUMANodes(logKey, createNUMANodeList(zones), resources, v1qos.GetPodQOS(pod), nodeInfo) {
		// definitely we can't align container, so we can't align a pod
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("cannot align pod: %s", pod.Name))
	}
	return nil
}

// Filter Now only single-numa-node supported
func (tm *TopologyMatch) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	if v1qos.GetPodQOS(pod) == v1.PodQOSBestEffort {
		return nil
	}

	nodeName := nodeInfo.Node().Name
	nodeTopology := findNodeTopology(nodeName, tm.lister)

	if nodeTopology == nil {
		return nil
	}

	klog.V(5).InfoS("Found NodeResourceTopology", "nodeTopology", klog.KObj(nodeTopology))
	for _, policyName := range nodeTopology.TopologyPolicies {
		if handler, ok := tm.policyHandlers[topologyv1alpha1.TopologyManagerPolicy(policyName)]; ok {
			if status := handler.filter(pod, nodeTopology.Zones, nodeInfo); status != nil {
				return status
			}
		} else {
			klog.V(5).InfoS("Policy handler not found", "policy", policyName)
		}
	}
	return nil
}

// resourceFoundOnNode checks whether a given resource exist at the node level
// and whether the given quantity is big enough
func resourceFoundOnNode(resName v1.ResourceName, wantQuantity resource.Quantity, nodeInfo *framework.NodeInfo) bool {
	resourceList := util.ResourceList(nodeInfo.Allocatable)
	if gotQuantity, ok := resourceList[resName]; ok {
		return gotQuantity.Cmp(wantQuantity) >= 0
	}
	return false
}
