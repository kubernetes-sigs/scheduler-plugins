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

func TestNUMANodesRequired(t *testing.T) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node",
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("10Gi"),
			},
		},
	}
	testCases := []struct {
		description     string
		numaNodes       NUMANodeList
		podResources    v1.ResourceList
		node            *v1.Node
		expectedErr     error
		expectedBitmask bitmask.BitMask
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
			node:            node,
			expectedBitmask: NewTestBitmask(0),
			expectedErr:     nil,
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
			node:            node,
			expectedBitmask: NewTestBitmask(0, 1),
			expectedErr:     nil,
		},
		{
			description: "no pod resources",
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
			podResources:    v1.ResourceList{},
			node:            node,
			expectedErr:     nil,
			expectedBitmask: bitmask.NewEmptyBitMask(),
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
			node:            node,
			expectedBitmask: nil,
			expectedErr:     fmt.Errorf("cannot calculate how many NUMA nodes are required for: test"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			bm := numaNodesRequired("test", v1.PodQOSGuaranteed, tc.numaNodes, tc.podResources)

			if bm != nil && !bm.IsEqual(tc.expectedBitmask) {
				t.Errorf("wrong bitmask expected: %d got: %d", tc.expectedBitmask, bm)
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
		description   string
		score         int
		expectedScore int64
	}{
		{
			description:   "1 numa node",
			score:         1,
			expectedScore: 88,
		},
		{
			description:   "2 numa nodes",
			score:         2,
			expectedScore: 76,
		},
		{
			description:   "8 numa nodes",
			score:         8,
			expectedScore: 4,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.description, func(t *testing.T) {
			normalizedScore := normalizeScore(tc.score)
			if normalizedScore != tc.expectedScore {
				t.Errorf("Expected normalizedScore to be %d not %d", tc.expectedScore, normalizedScore)
			}
		})
	}
}
