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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestSubstractNUMA(t *testing.T) {
	tcases := []struct {
		description string
		numaNodes   NUMANodeList
		nodes       []int
		resources   v1.ResourceList
		expected    NUMANodeList
	}{
		{
			description: "simple",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			resources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			nodes: []int{0},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(6, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
			},
		},
		{
			description: "substract resources from 2 NUMA nodes",
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
			resources: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewQuantity(12, resource.DecimalSI),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			nodes: []int{0, 1},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(0, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
				{
					NUMAID: 1,
					Resources: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			subtractFromNUMAs(tcase.resources, tcase.numaNodes, tcase.nodes...)
			for i, node := range tcase.numaNodes {
				for resName, quantity := range node.Resources {
					if !tcase.expected[i].Resources[resName].Equal(quantity) {
						t.Errorf("Expected %s to equal %v instead of %v", resName, tcase.expected[i].Resources[resName], quantity)
					}
				}
			}
		})
	}
}
