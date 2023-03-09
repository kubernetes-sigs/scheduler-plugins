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

package qos

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func createPodInfo(pod *v1.Pod) *framework.PodInfo {
	podInfo, _ := framework.NewPodInfo(pod)
	return podInfo
}

func TestSortLess(t *testing.T) {
	tests := []struct {
		name   string
		pInfo1 *framework.QueuedPodInfo
		pInfo2 *framework.QueuedPodInfo
		want   bool
	}{
		{
			name: "p1's priority greater than p2",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 100, nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 50, nil, nil)),
			},
			want: true,
		},
		{
			name: "p1's priority less than p2",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 50, nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 80, nil, nil)),
			},
			want: false,
		},
		{
			name: "p1 and p2 are both BestEfforts",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, nil, nil)),
			},
			want: true,
		},
		{
			name: "p1 is BestEfforts, p2 is Guaranteed",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, getResList("100m", "100Mi"), getResList("100m", "100Mi"))),
			},
			want: false,
		},
		{
			name: "p1 is Burstable, p2 is Guaranteed",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, getResList("100m", "100Mi"), getResList("200m", "200Mi"))),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, getResList("100m", "100Mi"), getResList("100m", "100Mi"))),
			},
			want: false,
		},
		{
			name: "both p1 and p2 are Burstable",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, getResList("100m", "100Mi"), getResList("200m", "200Mi"))),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, getResList("100m", "100Mi"), getResList("200m", "200Mi"))),
			},
			want: true,
		},
		{
			name: "p1 is Guaranteed, p2 is Burstable",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, getResList("100m", "100Mi"), getResList("100m", "100Mi"))),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, getResList("100m", "100Mi"), getResList("200m", "200Mi"))),
			},
			want: true,
		},
		{
			name: "both p1 and p2 are Guaranteed",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p1", 0, getResList("100m", "100Mi"), getResList("100m", "100Mi"))),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: createPodInfo(makePod("p2", 0, getResList("100m", "100Mi"), getResList("100m", "100Mi"))),
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sort{}
			if got := s.Less(tt.pInfo1, tt.pInfo2); got != tt.want {
				t.Errorf("Less() = %v, want %v", got, tt.want)
			}
		})
	}
}

func makePod(name string, priority int32, requests, limits v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Priority: &priority,
			Containers: []v1.Container{
				{
					Name: name,
					Resources: v1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
				},
			},
		},
	}
}

func getResList(cpu, memory string) v1.ResourceList {
	res := v1.ResourceList{}
	if cpu != "" {
		res[v1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		res[v1.ResourceMemory] = resource.MustParse(memory)
	}
	return res
}
