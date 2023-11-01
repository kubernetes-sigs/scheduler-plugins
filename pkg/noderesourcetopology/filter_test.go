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
	"reflect"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

const (
	cpu                        = string(v1.ResourceCPU)
	memory                     = string(v1.ResourceMemory)
	extended                   = "namespace/extended"
	hugepages2Mi               = "hugepages-2Mi"
	nicResourceName            = "vendor/nic1"
	notExistingNICResourceName = "vendor/notexistingnic"
	containerName              = "container1"
	nicResourceNameNoNUMA      = "vendor.com/old-nic-model"
)

type nodeTopologyDesc struct {
	nrt  *topologyv1alpha2.NodeResourceTopology
	node v1.ResourceList
}

func TestNodeResourceTopology(t *testing.T) {
	nodeTopologyDescs := []nodeTopologyDesc{
		{
			nrt: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
				TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
				Zones: topologyv1alpha2.ZoneList{
					{
						Name: "node-0",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "20", "4"),
							MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "10"),
						},
					},
					{
						Name: "node-1",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "30", "8"),
							MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "10"),
						},
					},
				},
			},
		},
		{
			nrt: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta:       metav1.ObjectMeta{Name: "node2"},
				TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
				Zones: topologyv1alpha2.ZoneList{
					{
						Name: "node-0",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "20", "2"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(hugepages2Mi, "128Mi", "128Mi"),
							MakeTopologyResInfo(nicResourceName, "30", "5"),
						},
					},
					{
						Name: "node-1",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "30", "4"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(hugepages2Mi, "128Mi", "128Mi"),
							MakeTopologyResInfo(nicResourceName, "30", "2"),
						},
					},
				},
			},
			node: v1.ResourceList{
				v1.ResourceName(nicResourceNameNoNUMA): resource.MustParse("4"),
			},
		},
		{
			nrt: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta:       metav1.ObjectMeta{Name: "node3"},
				TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodePodLevel)},
				Zones: topologyv1alpha2.ZoneList{
					{
						Name: "node-0",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "20", "2"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "5"),
						},
					},
					{
						Name: "node-1",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "30", "4"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "2"),
						},
					},
				},
			},
		},
		{
			nrt: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta:       metav1.ObjectMeta{Name: "badly_formed_node"},
				TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodePodLevel)},
				Zones: topologyv1alpha2.ZoneList{
					{
						Name: "node-0",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "20", "2"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "5"),
						},
					},
					{
						Name: "node-75",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "30", "4"),
							MakeTopologyResInfo(memory, "8Gi", "4Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "2"),
						},
					},
				},
			},
		},
		{
			nrt: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta:       metav1.ObjectMeta{Name: "extended"},
				TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
				Zones: topologyv1alpha2.ZoneList{
					{
						Name: "node-0",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "20", "4"),
							MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "10"),
						},
					},
					{
						Name: "node-1",
						Type: "Node",
						Resources: topologyv1alpha2.ResourceInfoList{
							MakeTopologyResInfo(cpu, "30", "8"),
							MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							MakeTopologyResInfo(nicResourceName, "30", "10"),
						},
					},
				},
			},
			node: v1.ResourceList{
				v1.ResourceName(extended): resource.MustParse("1"),
			},
		},
	}

	nodes := make([]*v1.Node, len(nodeTopologyDescs))
	for i := range nodes {
		nodeResTopology := nodeTopologyDescs[i].nrt
		res := makeResourceListFromZones(nodeResTopology.Zones)
		nodes[i] = &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeResTopology.Name},
			Status: v1.NodeStatus{
				Capacity:    res,
				Allocatable: res,
			},
		}

		for resName, resQty := range nodeTopologyDescs[i].node {
			nodes[i].Status.Capacity[resName] = resQty
			nodes[i].Status.Allocatable[resName] = resQty
		}
	}

	// Test different QoS Guaranteed/Burstable/BestEffort
	tests := []struct {
		name       string
		pod        *v1.Pod
		node       *v1.Node
		wantStatus *framework.Status
	}{
		{
			name: "Guaranteed QoS, pod with extended resource fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				extended:          resource.MustParse("1"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[4],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, pod with extended resource no devices; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				extended:          resource.MustParse("1")}),
			node:       nodes[4],
			wantStatus: nil,
		},
		{
			name:       "Best effort QoS, pod fit",
			pod:        &v1.Pod{},
			node:       nodes[0],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting devices, Pod Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting devices, Pod Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				nicResourceName: *resource.NewQuantity(20, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Best effort QoS requesting devices, Container Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting devices, Container Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				nicResourceName: *resource.NewQuantity(20, resource.DecimalSI)}),
			node:       nodes[0],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Best effort QoS requesting devices and extended resources, Container Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				extended:        resource.MustParse("1"),
				nicResourceName: *resource.NewQuantity(10, resource.DecimalSI)}),
			node:       nodes[4],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS, requesting CPU, memory (enough on NUMA) and devices (not enough), Container Scope Topology policy; pod doesn't fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("3Gi"),
					nicResourceName:   *resource.NewQuantity(11, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					nicResourceName:   *resource.NewQuantity(11, resource.DecimalSI)},
			),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Best effort QoS, requesting CPU, memory (enough on NUMA) and devices (not enough), Pod Scope Topology policy; pod doesn't fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(1, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("1Gi"),
					nicResourceName:   *resource.NewQuantity(6, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("2Gi"),
					nicResourceName:   *resource.NewQuantity(6, resource.DecimalSI)},
			),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Best effort QoS requesting CPU, memory (enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(1, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("1Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("2Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
			),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting CPU, memory (not enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(19, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("5Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(20, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("6Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
			),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting CPU, memory (enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("3Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
			),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Best effort QoS requesting CPU, memory (not enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodWithReqAndLimitByResourceList(
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(5, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("5Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
				&v1.ResourceList{
					v1.ResourceCPU:    *resource.NewQuantity(6, resource.DecimalSI),
					v1.ResourceMemory: resource.MustParse("6Gi"),
					nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)},
			),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, minimal, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi")}),
			node:       nodes[0],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, minimal, saturating zone, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    findAvailableResourceByName(nodeTopologyDescs[0].nrt.Zones[1].Resources, cpu),
				v1.ResourceMemory: findAvailableResourceByName(nodeTopologyDescs[0].nrt.Zones[1].Resources, memory)}),
			node:       nodes[0],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, zero quantity of unavailable resource, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				hugepages2Mi:      resource.MustParse("0"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[0],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, hugepages, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				hugepages2Mi:      resource.MustParse("64Mi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(4, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS, requesting CPU and devices (not enough), Container Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(4, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(11, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Burstable QoS, requesting CPU and devices (not enough), Pod Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(6, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Burstable QoS requesting CPU (enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "BurstableQoS requesting CPU (not enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(20, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting CPU (enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting CPU (not enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(4, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS, requesting memory (enough on NUMA) and devices (not enough), Container Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(11, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Burstable QoS, requesting memory (enough on NUMA) and devices (not enough), Pod Scope Topology policy; pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(6, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Burstable QoS requesting memory (enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting memory (not enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("5Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting memory (enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("4Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting memory (not enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("5Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS, requesting CPU, memory (enough on NUMA) and devices (not enough), Container Scope Topology policy; pod doesn't fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("4Gi"),
				nicResourceName:   *resource.NewQuantity(11, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Burstable QoS, requesting CPU, memory (enough on NUMA) and devices (not enough), Pod Scope Topology policy; pod doesn't fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(6, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Burstable QoS requesting CPU, memory (enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting CPU, memory (not enough on NUMA) and devices, Pod Scope Topology policy; pod fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(20, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("5Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting CPU, memory (enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("4Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS requesting CPU, memory (not enough on NUMA) and devices, Container Scope Topology policy; pod fit",
			pod: makePodWithReqByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(5, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("5Gi"),
				nicResourceName:   *resource.NewQuantity(5, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Burstable QoS with extended resources, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				extended:        resource.MustParse("1"),
				v1.ResourceCPU:  *resource.NewQuantity(4, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[4],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, hugepages, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				hugepages2Mi:      resource.MustParse("256Mi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Guaranteed QoS, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(9, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("1Gi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[0],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align container"),
		},
		{
			name: "Guaranteed QoS, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:             *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory:          resource.MustParse("1Gi"),
				notExistingNICResourceName: *resource.NewQuantity(0, resource.DecimalSI)}),
			node:       nodes[0],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS Topology Scope, pod doesn't fit",
			pod: makePodByResourceListWithManyContainers(&v1.ResourceList{
				v1.ResourceCPU:             *resource.NewQuantity(3, resource.DecimalSI),
				v1.ResourceMemory:          resource.MustParse("1Gi"),
				notExistingNICResourceName: *resource.NewQuantity(0, resource.DecimalSI)}, 3),
			node:       nodes[2],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Guaranteed QoS Topology Scope, minimal, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(1, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("1Gi")}),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS TopologyScope, minimal, saturating zone, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    findAvailableResourceByName(nodeTopologyDescs[3].nrt.Zones[0].Resources, cpu),
				v1.ResourceMemory: findAvailableResourceByName(nodeTopologyDescs[3].nrt.Zones[0].Resources, memory)}),
			node:       nodes[3],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS Topology Scope, pod fit",
			pod: makePodByResourceListWithManyContainers(&v1.ResourceList{
				v1.ResourceCPU:             *resource.NewQuantity(1, resource.DecimalSI),
				v1.ResourceMemory:          resource.MustParse("1Gi"),
				notExistingNICResourceName: *resource.NewQuantity(0, resource.DecimalSI)}, 3),
			node:       nodes[2],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS Topology Scope, invalid node",
			pod: makePodByResourceListWithManyContainers(&v1.ResourceList{
				v1.ResourceCPU:             *resource.NewQuantity(1, resource.DecimalSI),
				v1.ResourceMemory:          resource.MustParse("1Gi"),
				notExistingNICResourceName: *resource.NewQuantity(0, resource.DecimalSI)}, 3),
			node:       nodes[3],
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "Guaranteed QoS, hugepages, non-NUMA affine NIC, pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:        *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory:     resource.MustParse("2Gi"),
				hugepages2Mi:          resource.MustParse("64Mi"),
				nicResourceNameNoNUMA: *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil,
		},
		{
			name: "Guaranteed QoS, ephemeral-storage (non-NUMA), pod fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:              *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory:           resource.MustParse("2Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("100Mi"),
			}),
			node:       nodes[1],
			wantStatus: nil,
		},
	}

	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatalf("failed to create fake client: %v", err)
	}
	for _, desc := range nodeTopologyDescs {
		if err := fakeClient.Create(context.Background(), desc.nrt.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}

	tm := TopologyMatch{
		nrtCache: nrtcache.NewPassthrough(fakeClient),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(tt.node)
			if len(tt.pod.Spec.Containers) > 0 {
				tt.pod.Spec.Containers[0].Name = containerName
			}
			gotStatus := tm.Filter(context.Background(), framework.NewCycleState(), tt.pod, nodeInfo)

			if !reflect.DeepEqual(gotStatus, tt.wantStatus) {
				t.Errorf("status does not match: %v, want: %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

type resourceDescriptor struct {
	Host     string
	Node     string
	Resource string
	Quantity string
}

func TestNodeResourceTopologyMultiContainerPodScope(t *testing.T) {
	nodeTopologies := []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "host0"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodePodLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(hugepages2Mi, "384Mi", "384Mi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "32"),
						MakeTopologyResInfo(memory, "64Gi", "64Gi"),
						MakeTopologyResInfo(hugepages2Mi, "512Mi", "512Mi"),
						MakeTopologyResInfo(nicResourceName, "32", "32"),
					},
				},
			},
		},
	}

	nodes := make([]*v1.Node, len(nodeTopologies))
	for i := range nodes {
		nodes[i] = makeNodeFromNodeResourceTopology(nodeTopologies[i])
	}

	tests := []struct {
		name       string
		pod        *v1.Pod
		node       *v1.Node
		nrts       []*topologyv1alpha2.NodeResourceTopology
		avail      []resourceDescriptor
		wantStatus *framework.Status
	}{
		{
			name: "gu pod fits only on a numa node",
			pod: makePod("testpod",
				withMultiContainers([]v1.ResourceList{
					{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("26"),
						v1.ResourceMemory: resource.MustParse("32Gi"),
						hugepages2Mi:      resource.MustParse("512Mi"),
						nicResourceName:   resource.MustParse("26"),
					},
				},
				)),
			node: nodes[0],
			nrts: []*topologyv1alpha2.NodeResourceTopology{
				nodeTopologies[0],
			},
			avail:      []resourceDescriptor{},
			wantStatus: nil,
		},
		{
			name: "gu pod does not fit - not enough CPUs available on any NUMA node",
			pod: makePod("testpod",
				withMultiContainers([]v1.ResourceList{
					{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("8"),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("26"),
						v1.ResourceMemory: resource.MustParse("26Gi"),
						hugepages2Mi:      resource.MustParse("52Mi"),
						nicResourceName:   resource.MustParse("26"),
					},
				},
				)),
			node: nodes[0],
			nrts: []*topologyv1alpha2.NodeResourceTopology{
				nodeTopologies[0],
			},
			avail:      []resourceDescriptor{},
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "gu pod does not fit - not enough memory available on any NUMA node",
			pod: makePod("testpod",
				withMultiContainers([]v1.ResourceList{
					{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("4Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("16Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("26"),
						v1.ResourceMemory: resource.MustParse("52Gi"),
						hugepages2Mi:      resource.MustParse("52Mi"),
						nicResourceName:   resource.MustParse("26"),
					},
				},
				)),
			node: nodes[0],
			nrts: []*topologyv1alpha2.NodeResourceTopology{
				nodeTopologies[0],
			},
			avail:      []resourceDescriptor{},
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "gu pod does not fit - not enough Hugepages available on any NUMA node",
			pod: makePod("testpod",
				withMultiContainers([]v1.ResourceList{
					{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("26"),
						v1.ResourceMemory: resource.MustParse("32Gi"),
						hugepages2Mi:      resource.MustParse("3328Mi"), // 128Mi * 26
						nicResourceName:   resource.MustParse("26"),
					},
				},
				)),
			node: nodes[0],
			nrts: []*topologyv1alpha2.NodeResourceTopology{
				nodeTopologies[0],
			},
			avail:      []resourceDescriptor{},
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
		{
			name: "gu pod does not fit - not enough devices available on any NUMA node",
			pod: makePod("testpod",
				withMultiContainers([]v1.ResourceList{
					{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
					{
						v1.ResourceCPU:    resource.MustParse("26"),
						v1.ResourceMemory: resource.MustParse("26Gi"),
						hugepages2Mi:      resource.MustParse("52Mi"),
						nicResourceName:   resource.MustParse("52"),
					},
				},
				)),
			node: nodes[0],
			nrts: []*topologyv1alpha2.NodeResourceTopology{
				nodeTopologies[0],
			},
			avail:      []resourceDescriptor{},
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient, err := tu.NewFakeClient()
			if err != nil {
				t.Fatalf("failed to create fake client: %v", err)
			}
			for _, obj := range nodeTopologies {
				if err := fakeClient.Create(context.Background(), obj.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			}

			tm := TopologyMatch{
				nrtCache: nrtcache.NewPassthrough(fakeClient),
			}

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(tt.node)
			if len(tt.pod.Spec.Containers) > 0 {
				tt.pod.Spec.Containers[0].Name = containerName
			}
			gotStatus := tm.Filter(context.Background(), framework.NewCycleState(), tt.pod, nodeInfo)

			if !reflect.DeepEqual(gotStatus, tt.wantStatus) {
				t.Errorf("status does not match: %v, want: %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

// should be filled by the user
type testUserEntry struct {
	// description contains the test type and tier as described in TESTS.md
	// and a short description of the test itself
	description string
	initCntReq  []map[string]string
	cntReq      []map[string]string
	statusErr   string
	// this testing batch is going to br run against the same node and NRT objects, hence we're not specifying them.
}

// will be generated by parseTestUserEntry given a []testUserEntry
type testEntry struct {
	name       string
	pod        *v1.Pod
	wantStatus *framework.Status
}

func TestNodeResourceTopologyMultiContainerContainerScope(t *testing.T) {
	nodeTopologies := []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "host0"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(hugepages2Mi, "384Mi", "384Mi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "32"),
						MakeTopologyResInfo(memory, "64Gi", "64Gi"),
						MakeTopologyResInfo(hugepages2Mi, "512Mi", "512Mi"),
						MakeTopologyResInfo(nicResourceName, "32", "32"),
					},
				},
			},
		},
	}

	nodes := make([]*v1.Node, len(nodeTopologies))
	for i := range nodes {
		nodes[i] = makeNodeFromNodeResourceTopology(nodeTopologies[i])
	}

	tue := []testUserEntry{
		{
			description: "[1][tier3] single container with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4G"},
			},
		},
		{
			description: "[2][tier3] single container with cpu over allocation - fit",
			cntReq: []map[string]string{
				{cpu: "40", memory: "4G"},
			},
			statusErr: "cannot align container", // cnt-1
		},
		{
			description: "[2][tier3] single container with memory over allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "100G"},
			},
			statusErr: "cannot align container", // cnt-1
		},
		{
			description: "[2][tier3] single container with cpu and memory over allocation - fit",
			cntReq: []map[string]string{
				{cpu: "40", memory: "100G"},
			},
			statusErr: "cannot align container", // cnt-1
		},
		{
			description: "[4][tier2] multi-containers with good allocation, spread across NUMAs - fit",
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
			},
		},
		{
			description: "[4][tier1] multi containers with good devices and hugepages allocation, spread across NUMAs - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "6G", hugepages2Mi: "500Mi", nicResourceName: "16"},
				{cpu: "2", memory: "6G", hugepages2Mi: "50Mi", nicResourceName: "8"},
			},
		},
		{
			description: "[7][tier1] init container with cpu over allocation, multi-containers with good allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "40", memory: "40G"},
			},
			cntReq: []map[string]string{
				{cpu: "1", memory: "4G"},
				{cpu: "1", memory: "4G"},
			},
			statusErr: "cannot align init container", // cnt-1
		},
		{
			description: "[7][tier1] init container with memory over allocation, multi-containers with good allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "70G"},
			},
			cntReq: []map[string]string{
				{cpu: "1", memory: "4G"},
				{cpu: "1", memory: "4G"},
			},
			statusErr: "cannot align init container", // cnt-1
		},
		{
			description: "[11][tier1] init container with good allocation, multi-containers spread across NUMAs - fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
			},
		},
		{
			description: "[17][tier1] multi init containers with good allocation, multi-containers spread across NUMAs - fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10G"},
				{cpu: "4", memory: "10G"},
				{cpu: "4", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
				{cpu: "6", memory: "10G"},
			},
		},
		{
			description: "[24][tier1] multi init containers with good allocation, multi-containers with over cpu allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10G"},
				{cpu: "30", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "6G"},
			},
			statusErr: "cannot align container", // cnt-3
		},
		{
			description: "[27][tier1] multi init containers with good allocation, container with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10G"},
				{cpu: "30", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "35", memory: "40G"},
			},
			statusErr: "cannot align container", // cnt-1
		},
		{
			description: "[28][tier1] multi init containers with good allocation, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10G"},
				{cpu: "30", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
			},
		},
		{
			description: "[29][tier1] multi init containers when sum of their cpus requests (together) is over allocatable, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10G"},
				{cpu: "30", memory: "10G"},
				{cpu: "30", memory: "10G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
				{cpu: "2", memory: "6G"},
			},
		},
		{
			description: "[29][tier1] multi init containers when sum of their memory requests (together) is over allocatable, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "3", memory: "50G"},
				{cpu: "3", memory: "50G"},
				{cpu: "3", memory: "50G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "20", memory: "40G"},
				{cpu: "2", memory: "6G"},
			},
		},
		{
			description: "[32][tier1] multi init containers with over cpu allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "40", memory: "50G"},
				{cpu: "3", memory: "50G"},
				{cpu: "3", memory: "50G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "2", memory: "6G"},
			},
			statusErr: "cannot align init container", // cnt-1
		},
		{
			description: "[32][tier1] multi init containers with over memory allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "20", memory: "50G"},
				{cpu: "40", memory: "50G"},
				{cpu: "3", memory: "50G"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40G"},
				{cpu: "2", memory: "6G"},
			},
			statusErr: "cannot align init container", // cnt-2
		},
	}

	tests := parseTestUserEntry(tue)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient, err := tu.NewFakeClient()
			if err != nil {
				t.Fatalf("failed to create fake client: %v", err)
			}

			for _, obj := range nodeTopologies {
				if err := fakeClient.Create(context.Background(), obj.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			}

			tm := TopologyMatch{
				nrtCache: nrtcache.NewPassthrough(fakeClient),
			}

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(nodes[0])
			gotStatus := tm.Filter(context.Background(), framework.NewCycleState(), tt.pod, nodeInfo)

			if !reflect.DeepEqual(gotStatus, tt.wantStatus) {
				t.Errorf("status does not match: %v, want: %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

func makeNodeFromNodeResourceTopology(nrt *topologyv1alpha2.NodeResourceTopology) *v1.Node {
	res := makeResourceListFromZones(nrt.Zones)
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nrt.Name,
		},
		Status: v1.NodeStatus{
			Capacity:    res,
			Allocatable: res,
		},
	}
}

func findAvailableResourceByName(resourceInfoList topologyv1alpha2.ResourceInfoList, name string) resource.Quantity {
	for _, resourceInfo := range resourceInfoList {
		if resourceInfo.Name == name {
			return resourceInfo.Available
		}
	}
	return resource.MustParse("0")
}

func makePod(name string, options ...func(*v1.Pod)) *v1.Pod {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	for _, o := range options {
		o(&pod)
	}
	return &pod
}

func withMultiContainers(resourcesList []v1.ResourceList) func(*v1.Pod) {
	return func(pod *v1.Pod) {
		var containers []v1.Container

		for idx := range resourcesList {
			res := cloneResourceList(resourcesList[idx])
			containers = append(containers, v1.Container{
				Name: fmt.Sprintf("cnt-%d", idx+1),
				Resources: v1.ResourceRequirements{
					Requests: res,
					Limits:   res,
				},
			})
		}
		pod.Spec.Containers = containers
	}
}

func withMultiInitContainers(resourcesList []v1.ResourceList) func(*v1.Pod) {
	return func(pod *v1.Pod) {
		p := &v1.Pod{}
		f := withMultiContainers(resourcesList)
		f(p)
		pod.Spec.InitContainers = p.Spec.Containers
	}
}

func cloneResourceList(rl v1.ResourceList) v1.ResourceList {
	res := make(v1.ResourceList)
	for name, qty := range rl {
		res[name] = qty
	}
	return res
}

func parseTestUserEntry(entries []testUserEntry) []testEntry {
	var teList []testEntry
	for i, e := range entries {
		irl := parseContainerRes(e.initCntReq)
		rl := parseContainerRes(e.cntReq)
		pod := makePod(fmt.Sprintf("testpod%d", i), withMultiInitContainers(irl), withMultiContainers(rl))
		te := testEntry{
			name:       e.description,
			pod:        pod,
			wantStatus: parseState(e.statusErr),
		}
		teList = append(teList, te)
	}
	return teList
}

func parseContainerRes(cntRes []map[string]string) []v1.ResourceList {
	rll := []v1.ResourceList{}
	for i := 0; i < len(cntRes); i++ {
		resMap := cntRes[i]

		rl := v1.ResourceList{}
		for k, v := range resMap {
			rl[v1.ResourceName(k)] = resource.MustParse(v)
		}
		rll = append(rll, rl)
	}

	return rll
}

func parseState(error string) *framework.Status {
	if len(error) == 0 {
		return nil
	}

	return framework.NewStatus(framework.Unschedulable, error)
}
