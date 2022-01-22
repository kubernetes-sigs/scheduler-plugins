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

package util

import (
	"reflect"
	"testing"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func makeResourceList(cpu, mem int64) v1.ResourceList {
	return v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpu, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(mem, resource.BinarySI),
	}
}

func TestGetPodEffectiveRequest(t *testing.T) {
	tests := []struct {
		name                 string
		containerRequest     []v1.ResourceList
		initContainerRequest []v1.ResourceList
		want                 v1.ResourceList
	}{
		{
			name: "1 container",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
			},
			initContainerRequest: nil,
			want:                 makeResourceList(1, 1),
		},
		{
			name: "2 containers",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
				makeResourceList(2, 3),
			},
			initContainerRequest: nil,
			want:                 makeResourceList(3, 4),
		},
		{
			name: "2 containers and 1 init container",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
				makeResourceList(2, 3),
			},
			initContainerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
			},
			want: makeResourceList(3, 4),
		},
		{
			name: "2 containers and 1 init container with large cpu",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
				makeResourceList(2, 3),
			},
			initContainerRequest: []v1.ResourceList{
				makeResourceList(10, 1),
			},
			want: makeResourceList(10, 4),
		},
		{
			name: "2 containers and 2 init containers with large cpu or mem",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
				makeResourceList(2, 3),
			},
			initContainerRequest: []v1.ResourceList{
				makeResourceList(10, 1),
				makeResourceList(1, 10),
			},
			want: makeResourceList(10, 10),
		},
		{
			name: "2 containers and 2 init containers with only large cpu",
			containerRequest: []v1.ResourceList{
				makeResourceList(1, 1),
				makeResourceList(2, 3),
			},
			initContainerRequest: []v1.ResourceList{
				makeResourceList(10, 1),
				makeResourceList(1, 1),
			},
			want: makeResourceList(10, 4),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{}
			for _, request := range tt.containerRequest {
				pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
					Resources: v1.ResourceRequirements{
						Requests: request,
					},
				})
			}
			for _, request := range tt.initContainerRequest {
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, v1.Container{
					Resources: v1.ResourceRequirements{
						Requests: request,
					},
				})
			}
			if got := GetPodEffectiveRequest(pod); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetPodEffectiveRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}
