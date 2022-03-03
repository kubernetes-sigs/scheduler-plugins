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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	faketopologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"
)

const (
	cpu                        = string(v1.ResourceCPU)
	memory                     = string(v1.ResourceMemory)
	hugepages2Mi               = "hugepages-2Mi"
	nicResourceName            = "vendor/nic1"
	notExistingNICResourceName = "vendor/notexistingnic"
	containerName              = "container1"
)

func TestNodeResourceTopology(t *testing.T) {
	nodeTopologies := []*topologyv1alpha1.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "4"),
						MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "10"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "30", "8"),
						MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "10"),
					},
				},
			},
		},
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node2"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "2"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(hugepages2Mi, "128Mi", "128Mi"),
						MakeTopologyResInfo(nicResourceName, "30", "5"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "30", "4"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(hugepages2Mi, "128Mi", "128Mi"),
						MakeTopologyResInfo(nicResourceName, "30", "2"),
					},
				},
			},
		},
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node3"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodePodLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "2"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "5"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "30", "4"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "2"),
					},
				},
			},
		},
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "badly_formed_node"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodePodLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "2"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "5"),
					},
				},
				{
					Name: "node-75",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "30", "4"),
						MakeTopologyResInfo(memory, "8Gi", "4Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "2"),
					},
				},
			},
		},
	}

	nodes := make([]*v1.Node, len(nodeTopologies))
	for i := range nodes {
		nodeResTopology := nodeTopologies[i]
		res := makeResourceListFromZones(nodeResTopology.Zones)
		nodes[i] = &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeResTopology.Name},
			Status: v1.NodeStatus{
				Capacity:    res,
				Allocatable: res,
			},
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
			name:       "Best effort QoS, pod fit",
			pod:        &v1.Pod{},
			node:       nodes[0],
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
				v1.ResourceCPU:    findAvailableResourceByName(nodeTopologies[0].Zones[1].Resources, cpu),
				v1.ResourceMemory: findAvailableResourceByName(nodeTopologies[0].Zones[1].Resources, memory)}),
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
			name: "Burstable QoS, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(14, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: nil, // number of cpu is exceeded, but in case of burstable QoS for cpu resources we rely on fit.go
		},
		{
			name: "Burstable QoS, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:  *resource.NewQuantity(4, resource.DecimalSI),
				nicResourceName: *resource.NewQuantity(11, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, fmt.Sprintf("cannot align container: %s", containerName)),
		},
		{
			name: "Guaranteed QoS, hugepages, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				hugepages2Mi:      resource.MustParse("256Mi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[1],
			wantStatus: framework.NewStatus(framework.Unschedulable, fmt.Sprintf("cannot align container: %s", containerName)),
		},
		{
			name: "Guaranteed QoS, pod doesn't fit",
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(9, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("1Gi"),
				nicResourceName:   *resource.NewQuantity(3, resource.DecimalSI)}),
			node:       nodes[0],
			wantStatus: framework.NewStatus(framework.Unschedulable, fmt.Sprintf("cannot align container: %s", containerName)),
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
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod: "),
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
				v1.ResourceCPU:    findAvailableResourceByName(nodeTopologies[3].Zones[0].Resources, cpu),
				v1.ResourceMemory: findAvailableResourceByName(nodeTopologies[3].Zones[0].Resources, memory)}),
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
			wantStatus: framework.NewStatus(framework.Unschedulable, "cannot align pod: "),
		},
	}

	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	for _, obj := range nodeTopologies {
		fakeInformer.Informer().GetStore().Add(obj)
	}
	lister := fakeInformer.Lister()

	tm := TopologyMatch{
		lister:         lister,
		policyHandlers: newPolicyHandlerMap(),
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

func findAvailableResourceByName(resourceInfoList topologyv1alpha1.ResourceInfoList, name string) resource.Quantity {
	for _, resourceInfo := range resourceInfoList {
		if resourceInfo.Name == name {
			return resourceInfo.Available
		}
	}
	return resource.MustParse("0")
}
