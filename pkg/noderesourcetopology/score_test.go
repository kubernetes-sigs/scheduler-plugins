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
	"reflect"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

const (
	gpu = "gpu"
)

type nodeToScoreMap map[string]int64

type nrtFilterFn func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology

func nrtPassthrough(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
	return nrt
}

func initTest(nodeTopologies []*topologyv1alpha2.NodeResourceTopology, nrtFilter nrtFilterFn) (map[string]*v1.Node, ctrlclient.Client) {
	nodesMap := make(map[string]*v1.Node)

	// init node objects
	for _, nrt := range nodeTopologies {
		res := makeResourceListFromZones(nrt.Zones)
		nodesMap[nrt.Name] = &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nrt.Name},
			Status: v1.NodeStatus{
				Capacity:    res,
				Allocatable: res,
			},
		}
	}

	// init topology lister
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		panic(err)
	}

	for _, obj_ := range nodeTopologies {
		obj := nrtFilter(obj_)
		if obj == nil {
			continue
		}

		if err := fakeClient.Create(context.Background(), obj.DeepCopy()); err != nil {
			panic(err)
		}
	}

	return nodesMap, fakeClient
}

func TestNodeResourceScorePlugin(t *testing.T) {

	type podRequests struct {
		pod        *v1.Pod
		name       string
		wantStatus *framework.Status
	}
	pRequests := []podRequests{
		{
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(20*1024*1024, resource.DecimalSI)}),
			name:       "Pod1",
			wantStatus: nil,
		},
	}

	// Each testScenario will describe a set pod requests arrived sequentially to the scoring plugin.
	type testScenario struct {
		name      string
		wantedRes nodeToScoreMap
		requests  []podRequests
		strategy  scoreStrategyFn
	}

	tests := []testScenario{
		{
			// On 0-MaxNodeScore scale
			// Node2 resource fractions:
			// CPU Fraction: 2 / 2 = 100%
			// Memory Fraction: 20M / 50M = 40%
			// Node2 score:(100 + 40) / 2 = 70
			name:      "MostAllocated strategy",
			wantedRes: nodeToScoreMap{"Node2": 70},
			requests:  pRequests,
			strategy:  mostAllocatedScoreStrategy,
		},
		{
			// On 0-MaxNodeScore scale
			// Node3 resource fractions:
			// CPU Fraction: 2 / 6 = 33%
			// Memory Fraction: 20M / 60M = 33%
			// Node3 score: MaxNodeScore - (0.33 - 0.33) = MaxNodeScore
			name:      "BalancedAllocation strategy",
			wantedRes: nodeToScoreMap{"Node3": 100},
			requests:  pRequests,
			strategy:  balancedAllocationScoreStrategy,
		},
		{
			// On 0-MaxNodeScore scale
			// Node1 resource fractions:
			// CPU Fraction: 2 / 4 = 50%
			// Memory Fraction: 20M / 500M = 4%
			// Node1 score: ((100 - 50) + (100 - 4)) / 2 = 73
			name:      "LeastAllocated strategy",
			wantedRes: nodeToScoreMap{"Node1": 73},
			requests:  pRequests,
			strategy:  leastAllocatedScoreStrategy,
		},
	}

	for _, test := range tests {
		nodesMap, lister := initTest(defaultNUMANodes(withPolicy(topologyv1alpha2.SingleNUMANodeContainerLevel)), nrtPassthrough)
		t.Run(test.name, func(t *testing.T) {
			tm := &TopologyMatch{
				scoreStrategyFunc: test.strategy,
				nrtCache:          nrtcache.NewPassthrough(klog.Background(), lister),
			}

			for _, req := range test.requests {
				nodeToScore := make(nodeToScoreMap, len(nodesMap))
				for _, node := range nodesMap {
					score, gotStatus := tm.Score(
						context.Background(),
						framework.NewCycleState(),
						req.pod,
						node.ObjectMeta.Name)

					t.Logf("%v; %v; %v; score: %v; status: %v\n",
						test.name,
						req.name,
						node.ObjectMeta.Name,
						score,
						gotStatus)

					if !reflect.DeepEqual(gotStatus, req.wantStatus) {
						t.Errorf("status does not match: %v, want: %v\n", gotStatus, req.wantStatus)
					}
					nodeToScore[node.ObjectMeta.Name] = score
				}
				gotNode := findMaxScoreNode(nodeToScore)
				gotScore := nodeToScore[gotNode]
				t.Logf("%q: got node %q with score %d\n", test.name, gotNode, gotScore)
				for wantNode, wantScore := range test.wantedRes {
					if wantNode != gotNode {
						t.Errorf("failed to select the desired node: wanted: %q, got: %q", wantNode, gotNode)
					}

					if wantScore != gotScore {
						t.Errorf("wrong score for node %q: wanted: %d, got: %d", gotNode, wantScore, gotScore)
					}
				}
			}
		})
	}
}

func TestNodeResourceScorePluginLeastNUMA(t *testing.T) {
	testCases := []struct {
		name        string
		podRequests []v1.ResourceList
		wantedRes   nodeToScoreMap
		nodes       []*topologyv1alpha2.NodeResourceTopology
	}{
		{
			name: "container scope, one container case 1",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 94,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, one container case 2",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 82,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, one container case 3",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("6"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 82,
				"Node2": 0,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, two containers case 1",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 94,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, two containers case 2",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 82,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, two containers case 3",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("5"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 82,
				"Node2": 0,
				"Node3": 94,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "container scope, two containers non NUMA resource",
			podRequests: []v1.ResourceList{
				{
					"non-numa-resource": resource.MustParse("1"),
				},
				{
					"non-numa-resource": resource.MustParse("2"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 100,
				"Node2": 100,
				"Node3": 100,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortContainerLevel)),
		},
		{
			name: "pod scope, two containers case 1",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 82,
				"Node3": 82,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortPodLevel)),
		},
		{
			name: "pod scope, two containers case 2",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 82,
				"Node3": 82,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortPodLevel)),
		},
		{
			name: "pod scope, two containers case 3",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("5"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 82,
				"Node2": 0,
				"Node3": 82,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortPodLevel)),
		},
		{
			name: "pod scope, one containers non NUMA resource",
			podRequests: []v1.ResourceList{
				{
					"non-numa-resource": resource.MustParse("1"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 100,
				"Node2": 100,
				"Node3": 100,
			},
			nodes: defaultNUMANodes(withPolicy(topologyv1alpha2.BestEffortPodLevel)),
		},
		{
			name: "container scope, one container, 4 NUMA nodes, 1 gpu ",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
					gpu:               resource.MustParse("1"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 94,
				"Node2": 94,
			},
			nodes: fourNUMANodes(),
		},
		{
			name: "container scope, one container, 4 NUMA nodes, 2 gpu ",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
					gpu:               resource.MustParse("2"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 82,
				"Node2": 76,
			},
			nodes: fourNUMANodes(),
		},
		{
			name: "container scope, 2 containers, 4 NUMA nodes, 2 gpu in one container ",
			podRequests: []v1.ResourceList{
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
					gpu:               resource.MustParse("2"),
				},
				{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			wantedRes: nodeToScoreMap{
				"Node1": 82,
				"Node2": 76,
			},
			nodes: fourNUMANodes(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nodesMap, lister := initTest(tc.nodes, nrtPassthrough)

			tm := &TopologyMatch{
				scoreStrategyType: apiconfig.LeastNUMANodes,
				nrtCache:          nrtcache.NewPassthrough(klog.Background(), lister),
			}
			nodeToScore := make(nodeToScoreMap, len(nodesMap))
			pod := makePodByResourceLists(tc.podRequests...)

			for _, node := range nodesMap {
				score, gotStatus := tm.Score(
					context.Background(),
					framework.NewCycleState(),
					pod,
					node.Name)

				t.Logf("%v; %v; %v; score: %v; status: %v\n",
					tc.name,
					pod.GetName(),
					node.Name,
					score,
					gotStatus)

				nodeToScore[node.Name] = score
			}
			if !reflect.DeepEqual(nodeToScore, tc.wantedRes) {
				t.Errorf("scores for nodes are incorrect wanted: %v, got: %v", tc.wantedRes, nodeToScore)
			}

		})
	}
}

// when only a subset of nodes has NRT data available[1], prefer the nodes which have the NRT data over the other nodes;
// IOW, a node without NRT data available should always have score == 0
func TestNodeResourcePartialDataScorePlugin(t *testing.T) {
	type podRequests struct {
		pod        *v1.Pod
		name       string
		wantStatus *framework.Status
	}
	pRequests := []podRequests{
		{
			pod: makePodByResourceList(&v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(20*1024*1024, resource.DecimalSI)}),
			name:       "Pod1",
			wantStatus: nil,
		},
	}

	type testScenario struct {
		name      string
		wantedRes nodeToScoreMap
		requests  []podRequests
		strategy  scoreStrategyFn
		nrtFilter nrtFilterFn
	}

	tests := []testScenario{
		{
			name:      "No data at all, MostAllocated strategy",
			wantedRes: nodeToScoreMap{},
			requests:  pRequests,
			strategy:  mostAllocatedScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				return nil
			},
		},
		{
			name:      "No data at all, LeastAllocated strategy",
			wantedRes: nodeToScoreMap{},
			requests:  pRequests,
			strategy:  leastAllocatedScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				return nil
			},
		},
		{
			name:      "No data at all, BalancedAllocation strategy",
			wantedRes: nodeToScoreMap{},
			requests:  pRequests,
			strategy:  balancedAllocationScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				return nil
			},
		},
		{
			name:      "One node with NRT data, MostAllocated strategy",
			wantedRes: nodeToScoreMap{"Node1": 27},
			requests:  pRequests,
			strategy:  mostAllocatedScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				if nrt.Name != "Node1" {
					return nil
				}
				return nrt
			},
		},
		{
			name:      "One node with NRT data, LeastAllocated strategy",
			wantedRes: nodeToScoreMap{"Node1": 73},
			requests:  pRequests,
			strategy:  leastAllocatedScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				if nrt.Name != "Node1" {
					return nil
				}
				return nrt
			},
		},
		{
			name:      "One node with NRT data, BalancedAllocation strategy",
			wantedRes: nodeToScoreMap{"Node1": 89},
			requests:  pRequests,
			strategy:  balancedAllocationScoreStrategy,
			nrtFilter: func(nrt *topologyv1alpha2.NodeResourceTopology) *topologyv1alpha2.NodeResourceTopology {
				if nrt.Name != "Node1" {
					return nil
				}
				return nrt
			},
		},
	}

	for _, test := range tests {
		nodesMap, lister := initTest(defaultNUMANodes(withPolicy(topologyv1alpha2.SingleNUMANodeContainerLevel)), test.nrtFilter)
		t.Run(test.name, func(t *testing.T) {
			tm := &TopologyMatch{
				scoreStrategyFunc: test.strategy,
				nrtCache:          nrtcache.NewPassthrough(klog.Background(), lister),
			}

			for _, req := range test.requests {
				nodeToScore := make(nodeToScoreMap, len(nodesMap))
				for _, node := range nodesMap {
					score, gotStatus := tm.Score(
						context.Background(),
						framework.NewCycleState(),
						req.pod,
						node.ObjectMeta.Name)

					t.Logf("%v; %v; %v; score: %v; status: %v\n",
						test.name,
						req.name,
						node.ObjectMeta.Name,
						score,
						gotStatus)

					if !reflect.DeepEqual(gotStatus, req.wantStatus) {
						t.Errorf("status does not match: %v, want: %v\n", gotStatus, req.wantStatus)
					}
					nodeToScore[node.ObjectMeta.Name] = score
				}
				gotNode := findMaxScoreNode(nodeToScore)
				gotScore := nodeToScore[gotNode]
				t.Logf("%q: got node %q with score %d\n", test.name, gotNode, gotScore)
				for wantNode, wantScore := range test.wantedRes {
					if wantNode != gotNode {
						t.Errorf("failed to select the desired node: wanted: %q, got: %q", wantNode, gotNode)
					}

					if wantScore != gotScore {
						t.Errorf("wrong score for node %q: wanted: %d, got: %d", gotNode, wantScore, gotScore)
					}
				}
			}
		})
	}
}

// return the name of the node with the highest score
func findMaxScoreNode(nodeToScore nodeToScoreMap) string {
	max := int64(0)
	electedNode := ""

	for nodeName, score := range nodeToScore {
		if max <= score {
			max = score
			electedNode = nodeName
		}
	}
	return electedNode
}

type nrtUpdater func(nrt *topologyv1alpha2.NodeResourceTopology)

func withPolicy(policy topologyv1alpha2.TopologyManagerPolicy) nrtUpdater {
	return func(nrt *topologyv1alpha2.NodeResourceTopology) {
		nrt.TopologyPolicies = []string{string(policy)}
	}
}

func defaultNUMANodes(upds ...nrtUpdater) []*topologyv1alpha2.NodeResourceTopology {
	nrts := []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "Node1"},
			Zones: topologyv1alpha2.ZoneList{
				topologyv1alpha2.Zone{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "4", "4"),
						MakeTopologyResInfo(memory, "500Mi", "500Mi"),
					},
				},
				topologyv1alpha2.Zone{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "4", "4"),
						MakeTopologyResInfo(memory, "500Mi", "500Mi"),
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "Node2"},
			Zones: topologyv1alpha2.ZoneList{
				topologyv1alpha2.Zone{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
					},
				}, topologyv1alpha2.Zone{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "Node3"},
			Zones: topologyv1alpha2.ZoneList{
				topologyv1alpha2.Zone{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "6", "6"),
						MakeTopologyResInfo(memory, "60Mi", "60Mi"),
					},
				}, topologyv1alpha2.Zone{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "6", "6"),
						MakeTopologyResInfo(memory, "60Mi", "60Mi"),
					},
				},
			},
		},
	}
	for _, upd := range upds {
		for idx := range nrts {
			// make sure to mutate the original object in the collection
			upd(nrts[idx])
		}
	}
	return nrts
}

func fourNUMANodes() []*topologyv1alpha2.NodeResourceTopology {
	return []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "Node1"},
			TopologyPolicies: []string{string(topologyv1alpha2.BestEffortContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "1", "1"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 10,
						},
						{
							Name:  "node-1",
							Value: 12,
						},
						{
							Name:  "node-2",
							Value: 20,
						},
						{
							Name:  "node-3",
							Value: 20,
						},
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "1", "1"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 12,
						},
						{
							Name:  "node-1",
							Value: 10,
						},
						{
							Name:  "node-2",
							Value: 20,
						},
						{
							Name:  "node-3",
							Value: 20,
						},
					},
				},
				{
					Name: "node-2",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "0", "0"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 20,
						},
						{
							Name:  "node-1",
							Value: 20,
						},
						{
							Name:  "node-2",
							Value: 10,
						},
						{
							Name:  "node-3",
							Value: 12,
						},
					},
				},
				{
					Name: "node-3",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "0", "0"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 20,
						},
						{
							Name:  "node-1",
							Value: 20,
						},
						{
							Name:  "node-2",
							Value: 12,
						},
						{
							Name:  "node-3",
							Value: 10,
						},
					},
				},
			},
		},
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "Node2"},
			TopologyPolicies: []string{string(topologyv1alpha2.BestEffortContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "1", "1"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 10,
						},
						{
							Name:  "node-1",
							Value: 12,
						},
						{
							Name:  "node-2",
							Value: 20,
						},
						{
							Name:  "node-3",
							Value: 20,
						},
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "0", "0"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 12,
						},
						{
							Name:  "node-1",
							Value: 10,
						},
						{
							Name:  "node-2",
							Value: 20,
						},
						{
							Name:  "node-3",
							Value: 20,
						},
					},
				},
				{
					Name: "node-2",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "1", "1"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 20,
						},
						{
							Name:  "node-1",
							Value: 20,
						},
						{
							Name:  "node-2",
							Value: 10,
						},
						{
							Name:  "node-3",
							Value: 12,
						},
					},
				},
				{
					Name: "node-3",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "2", "2"),
						MakeTopologyResInfo(memory, "50Mi", "50Mi"),
						MakeTopologyResInfo(gpu, "0", "0"),
					},
					Costs: topologyv1alpha2.CostList{
						{
							Name:  "node-0",
							Value: 20,
						},
						{
							Name:  "node-1",
							Value: 20,
						},
						{
							Name:  "node-2",
							Value: 12,
						},
						{
							Name:  "node-3",
							Value: 10,
						},
					},
				},
			},
		},
	}
}
