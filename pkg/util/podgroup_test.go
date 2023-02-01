/*
Copyright 2020 The Kubernetes Authors.

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

	"k8s.io/kubernetes/pkg/apis/core"
)

func TestCreateMergePatch(t *testing.T) {
	tests := []struct {
		old      interface{}
		new      interface{}
		expected string
	}{
		{
			old: &core.Pod{
				Spec: core.PodSpec{
					Hostname: "test",
				},
			},
			new: &core.Pod{
				Status: core.PodStatus{
					Reason: "test",
				},
			},
			expected: `{"Spec":{"Hostname":""},"Status":{"Reason":"test"}}`,
		},

		{
			old: &core.Pod{
				Spec: core.PodSpec{
					Hostname: "test",
				},
				Status: core.PodStatus{
					Reason: "test1",
				},
			},
			new: &core.Pod{
				Status: core.PodStatus{
					Reason: "test",
				},
			},
			expected: `{"Spec":{"Hostname":""},"Status":{"Reason":"test"}}`,
		},
	}

	for _, tcase := range tests {
		patch, err := CreateMergePatch(tcase.old, tcase.new)
		if err != nil {
			t.Error(err)
		}
		if string(patch) != tcase.expected {
			t.Errorf("expected %v get %v", tcase.expected, string(patch))
		}
	}
}
