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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
)

const (
	gpuResource = "gpu"
)

func TestNUMANodesRequired(t *testing.T) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node",
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("10Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("10Gi"),
				gpuResource:       resource.MustParse("2"),
			},
		},
	}
	testCases := []struct {
		description         string
		numaNodes           NUMANodeList
		podResources        v1.ResourceList
		node                *v1.Node
		expectedErr         error
		expectedBitmask     bitmask.BitMask
		expectedMinDistance bool
	}{
		{
			description: "simple case, fit on 1 NUMA node",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0),
			expectedMinDistance: true,
			expectedErr:         nil,
		},
		{
			description: "simple case, fit on 2 NUMA node",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(12, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 1),
			expectedMinDistance: true,
			expectedErr:         nil,
		},
		{
			description: "can't fit",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(22, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			node:                node,
			expectedBitmask:     nil,
			expectedErr:         fmt.Errorf("cannot calculate how many NUMA nodes are required for: test"),
			expectedMinDistance: false,
		},
		{
			description: "4 NUMA node optimal distance",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10,
						1: 12,
						2: 20,
						3: 20,
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 12,
						1: 10,
						2: 20,
						3: 20,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						1: 20,
						2: 10,
						3: 12,
					},
				},
				{
					NUMAID: 3,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						1: 20,
						2: 10,
						3: 12,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 1),
			expectedErr:         nil,
			expectedMinDistance: true,
		},
		{
			description: "4 NUMA node non optimal distance",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10,
						1: 12,
						2: 20,
						3: 20,
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 12,
						1: 10,
						2: 20,
						3: 20,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						1: 20,
						2: 10,
						3: 12,
					},
				},
				{
					NUMAID: 3,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						1: 20,
						2: 12,
						3: 10,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 2),
			expectedErr:         nil,
			expectedMinDistance: false,
		},
		{
			description: "8 NUMA node optimal distance, not sorted ids",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10, 1: 20, 2: 40, 3: 30, 4: 20, 5: 30, 6: 50, 7: 40,
					},
				},
				{
					NUMAID: 3,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 30, 1: 40, 2: 20, 3: 10, 4: 30, 5: 20, 6: 40, 7: 50,
					},
				},
				{
					NUMAID: 5,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 30, 1: 20, 2: 50, 3: 20, 4: 50, 5: 10, 6: 50, 7: 40,
					},
				},
				{
					NUMAID: 7,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 40, 1: 50, 2: 30, 3: 50, 4: 20, 5: 40, 6: 30, 7: 10,
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20, 1: 10, 2: 30, 3: 40, 4: 50, 5: 20, 6: 40, 7: 50,
					},
				},
				{
					NUMAID: 6,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 50, 1: 40, 2: 20, 3: 40, 4: 30, 5: 50, 6: 10, 7: 30,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 40, 1: 30, 2: 10, 3: 20, 4: 40, 5: 50, 6: 20, 7: 30,
					},
				},
				{
					NUMAID: 4,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20, 1: 50, 2: 40, 3: 30, 4: 10, 5: 50, 6: 30, 7: 20,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("3"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 1, 5),
			expectedErr:         nil,
			expectedMinDistance: true,
		},
		{
			description: "8 NUMA node non optimal distance, not sorted ids, odd digit NUMA node without memory",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10, 1: 20, 2: 40, 3: 30, 4: 20, 5: 30, 6: 50, 7: 40,
					},
				},
				{
					NUMAID: 3,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 30, 1: 40, 2: 20, 3: 10, 4: 30, 5: 20, 6: 40, 7: 50,
					},
				},
				{
					NUMAID: 5,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 30, 1: 20, 2: 50, 3: 20, 4: 50, 5: 10, 6: 50, 7: 40,
					},
				},
				{
					NUMAID: 7,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 40, 1: 50, 2: 30, 3: 50, 4: 20, 5: 40, 6: 30, 7: 10,
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 20, 1: 10, 2: 30, 3: 40, 4: 50, 5: 20, 6: 40, 7: 50,
					},
				},
				{
					NUMAID: 6,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 50, 1: 40, 2: 20, 3: 40, 4: 30, 5: 50, 6: 10, 7: 30,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 40, 1: 30, 2: 10, 3: 20, 4: 40, 5: 50, 6: 20, 7: 30,
					},
				},
				{
					NUMAID: 4,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20, 1: 50, 2: 40, 3: 30, 4: 10, 5: 50, 6: 30, 7: 20,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("3"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(2, 4, 6),
			expectedErr:         nil,
			expectedMinDistance: false,
		},
		{
			description: "4 NUMA node optimal distance, non sequential ids",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10,
						2: 12,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 12,
						2: 10,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 4,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 10,
						6: 12,
					},
				},
				{
					NUMAID: 6,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 12,
						6: 10,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 2),
			expectedErr:         nil,
			expectedMinDistance: true,
		},
		{
			description: "4 NUMA node non optimal distance, non sequential ids",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10,
						2: 12,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 12,
						2: 10,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 4,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("0"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 10,
						6: 12,
					},
				},
				{
					NUMAID: 6,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 12,
						6: 10,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 6),
			expectedErr:         nil,
			expectedMinDistance: false,
		},
		{
			description: "4 NUMA node non optimal distance, non sequential ids, NUMA node-2 and node-6 without memory",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 10,
						2: 12,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 2,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 12,
						2: 10,
						4: 20,
						6: 20,
					},
				},
				{
					NUMAID: 4,
					Resources: v1.ResourceList{
						gpuResource:       resource.MustParse("1"),
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("5Gi"),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 10,
						6: 12,
					},
				},
				{
					NUMAID: 6,
					Resources: v1.ResourceList{
						gpuResource:    resource.MustParse("1"),
						v1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
					Costs: map[int]int{
						0: 20,
						2: 20,
						4: 12,
						6: 10,
					},
				},
			},
			podResources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
				gpuResource:       resource.MustParse("2"),
			},
			node:                node,
			expectedBitmask:     NewTestBitmask(0, 4),
			expectedErr:         nil,
			expectedMinDistance: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			bm, isMinDistance := numaNodesRequired("test", v1.PodQOSGuaranteed, tc.numaNodes, tc.podResources)

			if bm != nil && !bm.IsEqual(tc.expectedBitmask) {
				t.Errorf("wrong bitmask expected: %d got: %d", tc.expectedBitmask, bm)
			}

			if isMinDistance != tc.expectedMinDistance {
				t.Errorf("wrong isMinDistance expected: %t got: %t", tc.expectedMinDistance, isMinDistance)
			}
		})
	}
}

func NewTestBitmask(bits ...int) bitmask.BitMask {
	bm, _ := bitmask.NewBitMask(bits...)
	return bm
}

func TestNormalizeScore(t *testing.T) {
	tcases := []struct {
		description     string
		score           int
		expectedScore   int64
		optimalDistance bool
	}{
		{
			description:   "1 numa node, non optimal distance",
			score:         1,
			expectedScore: 88,
		},
		{
			description:   "2 numa nodes, non optimal distance",
			score:         2,
			expectedScore: 76,
		},
		{
			description:   "8 numa nodes, non optimal distance",
			score:         8,
			expectedScore: 4,
		},
		{
			description:     "1 numa node, optimal distance",
			score:           1,
			expectedScore:   94,
			optimalDistance: true,
		},
		{
			description:     "2 numa nodes, optimal distance",
			score:           2,
			expectedScore:   82,
			optimalDistance: true,
		},
		{
			description:     "8 numa nodes, optimal distance",
			score:           8,
			expectedScore:   10,
			optimalDistance: true,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.description, func(t *testing.T) {
			normalizedScore := normalizeScore(tc.score, tc.optimalDistance)
			if normalizedScore != tc.expectedScore {
				t.Errorf("Expected normalizedScore to be %d not %d", tc.expectedScore, normalizedScore)
			}
		})
	}
}

func TestMinDistance(t *testing.T) {
	numaNodes := NUMANodeList{
		{
			NUMAID: 0,
			Costs: map[int]int{
				0: 10,
				1: 12,
				2: 20,
				3: 20,
			},
		},
		{
			NUMAID: 1,
			Costs: map[int]int{
				0: 12,
				1: 10,
				2: 20,
				3: 20,
			},
		},
		{
			NUMAID: 2,
			Costs: map[int]int{
				0: 20,
				1: 20,
				2: 10,
				3: 12,
			},
		},
		{
			NUMAID: 3,
			Costs: map[int]int{
				0: 20,
				1: 20,
				2: 12,
				3: 10,
			},
		},
	}
	numaNodesNoCosts := NUMANodeList{
		{
			NUMAID: 0,
		},
		{
			NUMAID: 1,
		},
		{
			NUMAID: 2,
		},
		{
			NUMAID: 3,
		},
	}

	tcases := []struct {
		description  string
		combinations [][]int
		numaNodes    NUMANodeList
		expected     float32
	}{
		{
			description: "single numa node combination",
			combinations: [][]int{
				{
					0,
				},
				{
					1,
				},
				{
					2,
				},
				{
					3,
				},
			},
			numaNodes: numaNodes,
			expected:  10,
		},
		{
			description: "two numa node combination",
			combinations: [][]int{
				{
					0, 1,
				},
				{
					0, 2,
				},
				{
					0, 3,
				},
				{
					1, 2,
				},
				{
					1, 3,
				},
				{
					2, 3,
				},
			},
			numaNodes: numaNodes,
			expected:  11,
		},
		{
			description: "three numa node combination",
			combinations: [][]int{
				{
					0, 1, 2,
				},
				{
					1, 2, 3,
				},
				{
					0, 2, 3,
				},
			},
			numaNodes: numaNodes,
			expected:  14.888889,
		},
		{
			description: "two numa node combination, no costs",
			combinations: [][]int{
				{
					0, 1,
				},
				{
					0, 2,
				},
				{
					0, 3,
				},
				{
					1, 2,
				},
				{
					1, 3,
				},
				{
					2, 3,
				},
			},
			numaNodes: numaNodesNoCosts,
			expected:  255,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.description, func(t *testing.T) {
			distance := minAvgDistanceInCombinations(tc.numaNodes, tc.combinations)
			if distance != tc.expected {
				t.Errorf("Expected distance to be %f not %f", tc.expected, distance)
			}
		})
	}
}
