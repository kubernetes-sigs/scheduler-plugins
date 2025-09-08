/*
Copyright 2025 The Kubernetes Authors.

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

package util

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestIsSidecarInitContainer(t *testing.T) {
	always_ := v1.ContainerRestartPolicyAlways
	testCases := []struct {
		name     string
		cnt      *v1.Container
		expected bool
	}{
		{
			name:     "zero value",
			cnt:      &v1.Container{},
			expected: false,
		},
		{
			name: "explicit nil",
			cnt: &v1.Container{
				RestartPolicy: nil,
			},
			expected: false,
		},
		{
			name: "true sidecar container",
			cnt: &v1.Container{
				Name:          "init-1",
				RestartPolicy: &always_,
			},
			expected: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(string(testCase.name), func(t *testing.T) {
			got := IsSidecarInitContainer(testCase.cnt)
			if got != testCase.expected {
				t.Fatalf("expected %t to equal %t", got, testCase.expected)
			}
		})
	}
}
