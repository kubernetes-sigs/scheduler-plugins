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

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	bm "k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// The maximum number of NUMA nodes that Topology Manager allows is 8
// https://kubernetes.io/docs/tasks/administer-cluster/topology-manager/#known-limitations
const highestNUMAID = 8

type PolicyHandler func(pod *v1.Pod, zoneMap topologyv1alpha2.ZoneList) *framework.Status

func singleNUMAContainerLevelHandler(lh logr.Logger, pod *v1.Pod, zones topologyv1alpha2.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status {
	lh.V(5).Info("container level single NUMA node handler")

	// prepare NUMANodes list from zoneMap
	nodes := createNUMANodeList(lh, zones)
	qos := v1qos.GetPodQOS(pod)

	// Node() != nil already verified in Filter(), which is the only public entry point
	logNumaNodes(lh, "container handler NUMA resources", nodeInfo.Node().Name, nodes)

	// the init containers are running SERIALLY and BEFORE the normal containers.
	// https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#understanding-init-containers
	// therefore, we don't need to accumulate their resources together
	for _, initContainer := range pod.Spec.InitContainers {
		// TODO: handle sidecar explicitely (new kind)
		clh := lh.WithValues(logging.KeyContainer, initContainer.Name, logging.KeyContainerKind, logging.KindContainerInit)
		clh.V(6).Info("desired resources", stringify.ResourceListToLoggable(initContainer.Resources.Requests)...)

		_, match := resourcesAvailableInAnyNUMANodes(clh, nodes, initContainer.Resources.Requests, qos, nodeInfo)
		if !match {
			// we can't align init container, so definitely we can't align a pod
			clh.V(2).Info("cannot align container")
			return framework.NewStatus(framework.Unschedulable, "cannot align init container")
		}
	}

	for _, container := range pod.Spec.Containers {
		clh := lh.WithValues(logging.KeyContainer, container.Name, logging.KeyContainerKind, logging.KindContainerApp)
		clh.V(6).Info("container requests", stringify.ResourceListToLoggable(container.Resources.Requests)...)

		numaID, match := resourcesAvailableInAnyNUMANodes(clh, nodes, container.Resources.Requests, qos, nodeInfo)
		if !match {
			// we can't align container, so definitely we can't align a pod
			clh.V(2).Info("cannot align container")
			return framework.NewStatus(framework.Unschedulable, "cannot align container")
		}

		// subtract the resources requested by the container from the given NUMA.
		// this is necessary, so we won't allocate the same resources for the upcoming containers
		err := subtractResourcesFromNUMANodeList(clh, nodes, numaID, qos, container.Resources.Requests)
		if err != nil {
			// this is an internal error which should never happen
			return framework.NewStatus(framework.Error, "inconsistent resource accounting", err.Error())
		}
		clh.V(4).Info("container aligned", "numaCell", numaID)
	}
	return nil
}

// resourcesAvailableInAnyNUMANodes checks for sufficient resource and return the NUMAID that would be selected by Kubelet.
// this function requires NUMANodeList with properly populated NUMANode, NUMAID should be in range 0-63
func resourcesAvailableInAnyNUMANodes(lh logr.Logger, numaNodes NUMANodeList, resources v1.ResourceList, qos v1.PodQOSClass, nodeInfo *framework.NodeInfo) (int, bool) {
	numaID := highestNUMAID
	bitmask := bm.NewEmptyBitMask()
	// set all bits, each bit is a NUMA node, if resources couldn't be aligned
	// on the NUMA node, bit should be unset
	bitmask.Fill()

	nodeResources := util.ResourceList(nodeInfo.Allocatable)

	for resource, quantity := range resources {
		if quantity.IsZero() {
			// why bother? everything's fine from the perspective of this resource
			lh.V(4).Info("ignoring zero-qty resource request", "resource", resource)
			continue
		}

		if _, ok := nodeResources[resource]; !ok {
			// some resources may not expose NUMA affinity (device plugins, extended resources), but all resources
			// must be reported at node level; thus, if they are not present at node level, we can safely assume
			// we don't have the resource at all.
			lh.V(2).Info("early verdict: cannot meet request", "resource", resource, "suitable", "false")
			return numaID, false
		}

		// for each requested resource, calculate which NUMA slots are good fits, and then AND with the aggregated bitmask, IOW unset appropriate bit if we can't align resources, or set it
		// obvious, bits which are not in the NUMA id's range would be unset
		hasNUMAAffinity := false
		resourceBitmask := bm.NewEmptyBitMask()
		for _, numaNode := range numaNodes {
			numaQuantity, ok := numaNode.Resources[resource]
			if !ok {
				continue
			}

			hasNUMAAffinity = true
			if !isResourceSetSuitable(qos, resource, quantity, numaQuantity) {
				continue
			}

			resourceBitmask.Add(numaNode.NUMAID)
			lh.V(6).Info("feasible", "numaCell", numaNode.NUMAID, "resource", resource)
		}

		// non-native resources or ephemeral-storage may not expose NUMA affinity,
		// but since they are available at node level, this is fine
		if !hasNUMAAffinity && isHostLevelResource(resource) {
			lh.V(6).Info("resource available at host level (no NUMA affinity)", "resource", resource)
			continue
		}

		bitmask.And(resourceBitmask)
		if bitmask.IsEmpty() {
			lh.V(2).Info("early verdict", "resource", resource, "suitable", "false")
			return numaID, false
		}
	}
	// according to TopologyManager, the preferred NUMA affinity, is the narrowest one.
	// https://github.com/kubernetes/kubernetes/blob/v1.24.0-rc.1/pkg/kubelet/cm/topologymanager/policy.go#L155
	// in single-numa-node policy all resources should be allocated from a single NUMA,
	// which means that the lowest NUMA ID (with available resources) is the one to be selected by Kubelet.
	numaID = bitmask.GetBits()[0]

	// at least one NUMA node is available
	ret := !bitmask.IsEmpty()
	lh.V(2).Info("final verdict", "suitable", ret, "numaCell", numaID)
	return numaID, ret
}

func singleNUMAPodLevelHandler(lh logr.Logger, pod *v1.Pod, zones topologyv1alpha2.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status {
	lh.V(5).Info("pod level single NUMA node handler")

	resources := util.GetPodEffectiveRequest(pod)

	nodes := createNUMANodeList(lh, zones)

	// Node() != nil already verified in Filter(), which is the only public entry point
	logNumaNodes(lh, "pod handler NUMA resources", nodeInfo.Node().Name, nodes)
	lh.V(6).Info("pod desired resources", stringify.ResourceListToLoggable(resources)...)

	numaID, match := resourcesAvailableInAnyNUMANodes(lh, createNUMANodeList(lh, zones), resources, v1qos.GetPodQOS(pod), nodeInfo)
	if !match {
		lh.V(2).Info("cannot align pod", "name", pod.Name)
		return framework.NewStatus(framework.Unschedulable, "cannot align pod")
	}
	lh.V(4).Info("all container placed", "numaCell", numaID)
	return nil
}

// Filter Now only single-numa-node supported
func (tm *TopologyMatch) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	if v1qos.GetPodQOS(pod) == v1.PodQOSBestEffort && !resourcerequests.IncludeNonNative(pod) {
		return nil
	}

	nodeName := nodeInfo.Node().Name

	lh := klog.FromContext(ctx).WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)

	lh.V(4).Info(logging.FlowBegin)
	defer lh.V(4).Info(logging.FlowEnd)

	nodeTopology, info := tm.nrtCache.GetCachedNRTCopy(ctx, nodeName, pod)
	lh = lh.WithValues(logging.KeyGeneration, info.Generation)
	if !info.Fresh {
		lh.V(2).Info("invalid topology data")
		return framework.NewStatus(framework.Unschedulable, "invalid node topology data")
	}
	if nodeTopology == nil {
		return nil
	}

	conf := nodeconfig.TopologyManagerFromNodeResourceTopology(lh, nodeTopology)

	lh.V(4).Info("found nrt data", "object", stringify.NodeResourceTopologyResources(nodeTopology), "conf", conf.String())

	handler := filterHandlerFromTopologyManager(conf)
	if handler == nil {
		return nil
	}
	status := handler(lh, pod, nodeTopology.Zones, nodeInfo)
	if status != nil {
		tm.nrtCache.NodeMaybeOverReserved(nodeName, pod)
	}
	return status
}

func filterHandlerFromTopologyManager(conf nodeconfig.TopologyManager) filterFn {
	if conf.Policy != kubeletconfig.SingleNumaNodeTopologyManagerPolicy {
		return nil
	}
	if conf.Scope == kubeletconfig.PodTopologyManagerScope {
		return singleNUMAPodLevelHandler
	}
	if conf.Scope == kubeletconfig.ContainerTopologyManagerScope {
		return singleNUMAContainerLevelHandler
	}
	return nil // cannot happen
}
