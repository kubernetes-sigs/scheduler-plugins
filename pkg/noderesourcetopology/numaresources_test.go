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
	"testing"

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
