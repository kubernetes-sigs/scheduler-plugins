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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2/ktesting"

	corev1 "k8s.io/api/core/v1"
)

func TestIsHostLevelResource(t *testing.T) {
	testCases := []struct {
		resource corev1.ResourceName
		expected bool
	}{
		{
			resource: corev1.ResourceCPU,
			expected: false,
		},
		{
			resource: corev1.ResourceMemory,
			expected: false,
		},
		{
			resource: corev1.ResourceName("hugepages-1Gi"),
			expected: false,
		},
		{
			resource: corev1.ResourceStorage,
			expected: true,
		},
		{
			resource: corev1.ResourceEphemeralStorage,
			expected: true,
		},
		{
			resource: corev1.ResourceName("vendor.io/fastest-nic"),
			expected: true,
		},
		{
			resource: corev1.ResourceName("awesome.com/gpu-for-ai"),
			expected: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(string(testCase.resource), func(t *testing.T) {
			got := isHostLevelResource(testCase.resource)
			if got != testCase.expected {
				t.Fatalf("expected %t to equal %t", got, testCase.expected)
			}
		})
	}
}

func TestIsNUMAAffineResource(t *testing.T) {
	testCases := []struct {
		resource corev1.ResourceName
		expected bool
	}{
		{
			resource: corev1.ResourceCPU,
			expected: true,
		},
		{
			resource: corev1.ResourceMemory,
			expected: true,
		},
		{
			resource: corev1.ResourceName("hugepages-1Gi"),
			expected: true,
		},
		{
			resource: corev1.ResourceStorage,
			expected: false,
		},
		{
			resource: corev1.ResourceEphemeralStorage,
			expected: false,
		},
		{
			resource: corev1.ResourceName("vendor.io/fastest-nic"),
			expected: false,
		},
		{
			resource: corev1.ResourceName("awesome.com/gpu-for-ai"),
			expected: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(string(testCase.resource), func(t *testing.T) {
			got := isNUMAAffineResource(testCase.resource)
			if got != testCase.expected {
				t.Fatalf("expected %t to equal %t", got, testCase.expected)
			}
		})
	}
}

func TestSubtractResourcesFromNUMANodeList(t *testing.T) {
	testCases := []struct {
		name          string
		nodes         NUMANodeList
		numaID        int
		qos           corev1.PodQOSClass
		containerRes  corev1.ResourceList
		expected      NUMANodeList
		expectedError error
	}{
		{
			name: "empty from empty",
			nodes: NUMANodeList{
				{
					NUMAID:    0,
					Resources: corev1.ResourceList{},
				},
			},
			numaID:       0,
			qos:          corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{},
			expected: NUMANodeList{
				{
					NUMAID:    0,
					Resources: corev1.ResourceList{},
				},
			},
		},
		{
			name: "inconsistent numaID",
			nodes: NUMANodeList{
				{
					NUMAID:    0,
					Resources: corev1.ResourceList{},
				},
			},
			numaID:       2,
			qos:          corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{},
			expected: NUMANodeList{
				{
					NUMAID:    0,
					Resources: corev1.ResourceList{},
				},
			},
		},
		{
			name: "empty from minimal",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "2"),
						corev1.ResourceMemory: mustParseQuantity(t, "4Gi"),
					},
				},
			},
			numaID:       0,
			qos:          corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "2"),
						corev1.ResourceMemory: mustParseQuantity(t, "4Gi"),
					},
				},
			},
		},
		{
			name: "remove core resources (GU qos)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "8"),
						corev1.ResourceMemory: mustParseQuantity(t, "16Gi"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:    mustParseQuantity(t, "2"),
				corev1.ResourceMemory: mustParseQuantity(t, "4Gi"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "6"),
						corev1.ResourceMemory: mustParseQuantity(t, "12Gi"),
					},
				},
			},
		},
		{
			name: "remove only devices resources (BU qos)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:                   mustParseQuantity(t, "8"),
						corev1.ResourceMemory:                mustParseQuantity(t, "16Gi"),
						corev1.ResourceName("vendor.io/gpu"): mustParseQuantity(t, "4"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSBurstable,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:                   mustParseQuantity(t, "2"),
				corev1.ResourceMemory:                mustParseQuantity(t, "4Gi"),
				corev1.ResourceName("vendor.io/gpu"): mustParseQuantity(t, "2"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:                   mustParseQuantity(t, "8"),
						corev1.ResourceMemory:                mustParseQuantity(t, "16Gi"),
						corev1.ResourceName("vendor.io/gpu"): mustParseQuantity(t, "2"),
					},
				},
			},
		},
		{
			name: "skip hostlevel resources (GU qos)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:                   mustParseQuantity(t, "8"),
						corev1.ResourceMemory:                mustParseQuantity(t, "16Gi"),
						corev1.ResourceName("vendor.io/nic"): mustParseQuantity(t, "4"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:                   mustParseQuantity(t, "6"),
				corev1.ResourceMemory:                mustParseQuantity(t, "12Gi"),
				corev1.ResourceName("vendor.io/nic"): mustParseQuantity(t, "2"),
				corev1.ResourceEphemeralStorage:      mustParseQuantity(t, "1Gi"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:                   mustParseQuantity(t, "2"),
						corev1.ResourceMemory:                mustParseQuantity(t, "4Gi"),
						corev1.ResourceName("vendor.io/nic"): mustParseQuantity(t, "2"),
					},
				},
			},
		},
		{
			name: "remove excessive core resources (GU qos)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "8"),
						corev1.ResourceMemory: mustParseQuantity(t, "16Gi"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:    mustParseQuantity(t, "10"),
				corev1.ResourceMemory: mustParseQuantity(t, "20Gi"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "8"),
						corev1.ResourceMemory: mustParseQuantity(t, "16Gi"),
					},
				},
			},
			expectedError: fmt.Errorf("resource %q request %s exceeds NUMA %d availability %s", corev1.ResourceCPU, "10", 0, "8"),
		},
		{
			name: "require missing resources (GU qos, device)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "8"),
						corev1.ResourceMemory: mustParseQuantity(t, "16Gi"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:                   mustParseQuantity(t, "4"),
				corev1.ResourceMemory:                mustParseQuantity(t, "8Gi"),
				corev1.ResourceName("vendor.io/gpu"): mustParseQuantity(t, "2"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "4"),
						corev1.ResourceMemory: mustParseQuantity(t, "8Gi"),
					},
				},
			},
		},
		{
			name: "require missing resources (GU qos, core)",
			nodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "8"),
						corev1.ResourceMemory: mustParseQuantity(t, "16Gi"),
					},
				},
			},
			numaID: 0,
			qos:    corev1.PodQOSGuaranteed,
			containerRes: corev1.ResourceList{
				corev1.ResourceCPU:                   mustParseQuantity(t, "4"),
				corev1.ResourceMemory:                mustParseQuantity(t, "8Gi"),
				corev1.ResourceName("hugepages-1Gi"): mustParseQuantity(t, "2Gi"),
			},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    mustParseQuantity(t, "4"),
						corev1.ResourceMemory: mustParseQuantity(t, "8Gi"),
					},
				},
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(string(testCase.name), func(t *testing.T) {
			nodes := testCase.nodes.DeepCopy()
			logh := ktesting.NewLogger(t, ktesting.DefaultConfig)
			err := subtractResourcesFromNUMANodeList(logh, nodes, testCase.numaID, testCase.qos, testCase.containerRes)
			if (err != nil) != (testCase.expectedError != nil) {
				t.Fatalf("got err %v expected err %v", err, testCase.expectedError)
			}
			if err == nil && !nodes.Equal(testCase.expected) {
				t.Errorf("got %#v expected %#v", nodes, testCase.expected)
			}
		})
	}
}

func TestSubstractNUMA(t *testing.T) {
	tcases := []struct {
		description string
		numaNodes   NUMANodeList
		nodes       []int
		resources   corev1.ResourceList
		expected    NUMANodeList
	}{
		{
			description: "simple",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			resources: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			nodes: []int{0},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(6, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
			},
		},
		{
			description: "substract resources from 2 NUMA nodes",
			numaNodes: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
				{
					NUMAID: 1,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("10Gi"),
					},
				},
			},
			resources: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(12, resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			nodes: []int{0, 1},
			expected: NUMANodeList{
				{
					NUMAID: 0,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(0, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
				},
				{
					NUMAID: 1,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						corev1.ResourceMemory: resource.MustParse("10Gi"),
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

func mustParseQuantity(t *testing.T, str string) resource.Quantity {
	qty, err := resource.ParseQuantity(str)
	if err != nil {
		t.Fatalf("parsing error: %v", err)
	}
	return qty
}
