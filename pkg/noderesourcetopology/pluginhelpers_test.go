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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

func TestGetID(t *testing.T) {
	testCases := []struct {
		description string
		name        string
		expectedID  int
		expectedErr error
	}{
		{
			description: "id equals 1",
			name:        "node-1",
			expectedID:  1,
		},
		{
			description: "id equals 10",
			name:        "node-10",
			expectedID:  10,
		},
		{
			description: "invalid format of name, node name without hyphen",
			name:        "node0",
			expectedErr: fmt.Errorf("invalid zone format"),
		},
		{
			description: "invalid format of name, zone instead of node",
			name:        "zone-10",
			expectedErr: fmt.Errorf("invalid zone format"),
		},
		{
			description: "invalid format of name, suffix is not an integer",
			name:        "node-a10a",
			expectedErr: fmt.Errorf("invalid zone format"),
		},
		{
			description: "invalid format of name, suffix is not an integer",
			name:        "node-10a",
			expectedErr: fmt.Errorf("invalid zone format"),
		},
		{
			description: "invalid numaID range",
			name:        "node-10123412415115114",
			expectedErr: fmt.Errorf("invalid NUMA id range"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			id, err := getID(testCase.name)
			if testCase.expectedErr == nil {
				if err != nil {
					t.Fatalf("expected err to be nil not %v", err)
				}

				if id != testCase.expectedID {
					t.Fatalf("expected id to equal %d not %d", testCase.expectedID, id)
				}
			} else {
				fmt.Println(id)
				if !strings.Contains(err.Error(), testCase.expectedErr.Error()) {
					t.Fatalf("expected err: %v to contain %s", err, testCase.expectedErr)
				}
			}
		})
	}
}

func TestOnlyNonNUMAResources(t *testing.T) {
	numaNodes := NUMANodeList{
		{
			NUMAID: 0,
			Resources: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse("10Gi"),
				"gpu":                 resource.MustParse("1"),
			},
		},
		{
			NUMAID: 1,
			Resources: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse("10Gi"),
				"nic":                 resource.MustParse("1"),
			},
		},
	}
	testCases := []struct {
		description string
		resources   corev1.ResourceList
		expected    bool
	}{
		{
			description: "all resources missing in NUMANodeList",
			resources: corev1.ResourceList{
				"resource1": resource.MustParse("1"),
				"resource2": resource.MustParse("1"),
			},
			expected: true,
		},
		{
			description: "resource is present in both NUMA nodes",
			resources: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
			expected: false,
		},
		{
			description: "more than resource is present in both NUMA nodes",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1"),
			},
			expected: false,
		},
		{
			description: "resource is present only in NUMA node 0",
			resources: corev1.ResourceList{
				"gpu": resource.MustParse("1"),
			},
			expected: false,
		},
		{
			description: "resource is present only in NUMA node 1",
			resources: corev1.ResourceList{
				"nic": resource.MustParse("1"),
			},
			expected: false,
		},
		{
			description: "two distinct resources from different NUMA nodes",
			resources: corev1.ResourceList{
				"nic": resource.MustParse("1"),
				"gpu": resource.MustParse("1"),
			},
			expected: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			result := onlyNonNUMAResources(numaNodes, testCase.resources)
			if result != testCase.expected {
				t.Fatalf("expected %t to equal %t", result, testCase.expected)
			}
		})
	}
}

func TestGetForeignPodsDetectMode(t *testing.T) {
	detectAll := apiconfig.ForeignPodsDetectAll
	detectNone := apiconfig.ForeignPodsDetectNone
	detectOnlyExclusiveResources := apiconfig.ForeignPodsDetectOnlyExclusiveResources

	testCases := []struct {
		description string
		cfg         *apiconfig.NodeResourceTopologyCache
		expected    apiconfig.ForeignPodsDetectMode
	}{
		{
			description: "nil config",
			expected:    apiconfig.ForeignPodsDetectAll,
		},
		{
			description: "empty config",
			cfg:         &apiconfig.NodeResourceTopologyCache{},
			expected:    apiconfig.ForeignPodsDetectAll,
		},
		{
			description: "explicit all",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ForeignPodsDetect: &detectAll,
			},
			expected: apiconfig.ForeignPodsDetectAll,
		},
		{
			description: "explicit disable",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ForeignPodsDetect: &detectNone,
			},
			expected: apiconfig.ForeignPodsDetectNone,
		},
		{
			description: "explicit OnlyExclusiveResources",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ForeignPodsDetect: &detectOnlyExclusiveResources,
			},
			expected: apiconfig.ForeignPodsDetectOnlyExclusiveResources,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			got := getForeignPodsDetectMode(testCase.cfg)
			if got != testCase.expected {
				t.Errorf("foreign pods detect mode got %v expected %v", got, testCase.expected)
			}
		})
	}
}
