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

package capacityscheduling

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func TestReserveResource(t *testing.T) {
	tests := []struct {
		before   *ElasticQuotaInfo
		name     string
		pods     []*v1.Pod
		expected *ElasticQuotaInfo
	}{
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 1000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 2,
					},
				},
			},
			name: "ElasticQuotaInfo ReserveResource",
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 1000, 1, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 100, 2000, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 0, 0, 2, midPriority, "t1-p3", "node-a"),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 4000,
					Memory:   350,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elasticQuotaInfo := tt.before
			for _, pod := range tt.pods {
				request := computePodResourceRequest(pod)
				elasticQuotaInfo.reserveResource(*request)
			}

			if !reflect.DeepEqual(elasticQuotaInfo, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected.Used, elasticQuotaInfo.Used)
			}
		})
	}
}

func TestUnReserveResource(t *testing.T) {
	tests := []struct {
		before   *ElasticQuotaInfo
		name     string
		pods     []*v1.Pod
		expected *ElasticQuotaInfo
	}{
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo UnReserveResource",
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 1000, 1, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 100, 2000, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 0, 0, 2, midPriority, "t1-p3", "node-a"),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 1000,
					Memory:   50,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 2,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elasticQuotaInfo := tt.before
			for _, pod := range tt.pods {
				request := computePodResourceRequest(pod)
				elasticQuotaInfo.unreserveResource(*request)
			}

			if !reflect.DeepEqual(elasticQuotaInfo, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected.Used, elasticQuotaInfo.Used)
			}
		})
	}
}

func TestUsedOverMinWith(t *testing.T) {
	tests := []struct {
		before     *ElasticQuotaInfo
		name       string
		podRequest *framework.Resource
		expected   bool
	}{
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Min: &framework.Resource{
					MilliCPU: 3000,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMinWith Sum Of PodRequest And Used Is Bigger Than Min",
			podRequest: &framework.Resource{
				MilliCPU: 1500,
				Memory:   100,
			},
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 300,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Min: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMinWith Sum Of PodRequest And Used Is Not Bigger Than Min",
			podRequest: &framework.Resource{
				MilliCPU: 100,
				Memory:   50,
			},
			expected: false,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 3900,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMinWith ElasticQuotaInfo Doesn't Have Max",
			podRequest: &framework.Resource{
				MilliCPU: 100,
				Memory:   100,
			},
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 10,
					Memory:   10,
				},
				Min: &framework.Resource{
					MilliCPU: 3000,
					Memory:   100,
				},
			},
			name: "ElasticQuotaInfo OverMinWith Used And Min Don't Have GPU Value",
			podRequest: &framework.Resource{
				MilliCPU: 10,
				Memory:   10,
				ScalarResources: map[v1.ResourceName]int64{
					ResourceGPU: 5,
				},
			},
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU:         10,
					Memory:           10,
					EphemeralStorage: 10,
				},
				Min: &framework.Resource{
					MilliCPU:         3000,
					Memory:           100,
					EphemeralStorage: 100,
				},
			},
			name: "ElasticQuotaInfo OverMinWith EphemeralStorage",
			podRequest: &framework.Resource{
				MilliCPU:         10,
				Memory:           10,
				EphemeralStorage: 10,
			},
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elasticQuotaInfo := tt.before
			podRequest := tt.podRequest
			actual := elasticQuotaInfo.usedOverMinWith(podRequest)
			if actual != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, actual)
			}
		})
	}
}

func TestUsedOverMaxWith(t *testing.T) {
	tests := []struct {
		before     *ElasticQuotaInfo
		name       string
		podRequest *framework.Resource
		expected   bool
	}{
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Max: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMaxWith Sum Of PodRequest And Used Is Bigger Than Max",
			podRequest: &framework.Resource{
				MilliCPU: 100,
				Memory:   100,
			},
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 3900,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Max: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMaxWith Sum Of PodRequest And Used Is Not Bigger Than Max",
			podRequest: &framework.Resource{
				MilliCPU: 100,
				Memory:   100,
			},
			expected: false,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 3900,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name: "ElasticQuotaInfo OverMaxWith ElasticQuotaInfo Doesn't Have Max",
			podRequest: &framework.Resource{
				MilliCPU: 100,
				Memory:   100,
			},
			expected: false,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 10,
					Memory:   10,
				},
				Max: &framework.Resource{
					MilliCPU: 3000,
					Memory:   100,
				},
			},
			name: "ElasticQuotaInfo OverMaxWith Used And Max Don't Have GPU Value",
			podRequest: &framework.Resource{
				MilliCPU: 10,
				Memory:   10,
				ScalarResources: map[v1.ResourceName]int64{
					ResourceGPU: 5,
				},
			},
			expected: false,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU:         10,
					Memory:           10,
					EphemeralStorage: 10,
				},
				Max: &framework.Resource{
					MilliCPU:         3000,
					Memory:           100,
					EphemeralStorage: 100,
				},
			},
			name: "ElasticQuotaInfo OverMaxWith EphemeralStorage",
			podRequest: &framework.Resource{
				MilliCPU:         10,
				Memory:           10,
				EphemeralStorage: 100,
			},
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elasticQuotaInfo := tt.before
			podRequest := tt.podRequest
			actual := elasticQuotaInfo.usedOverMaxWith(podRequest)
			if actual != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, actual)
			}
		})
	}
}

func TestUsedOverMin(t *testing.T) {
	tests := []struct {
		before   *ElasticQuotaInfo
		name     string
		expected bool
	}{
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Min: &framework.Resource{
					MilliCPU: 3000,
					Memory:   300,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name:     "ElasticQuotaInfo OverMin Used Is Bigger Than Min",
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 300,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Min: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name:     "ElasticQuotaInfo OverMin Used Is Not Bigger Than Min",
			expected: false,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 3900,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
			},
			name:     "ElasticQuotaInfo OverMin ElasticQuotaInfo Doesn't Have Min",
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU: 300,
					Memory:   100,
					ScalarResources: map[v1.ResourceName]int64{
						ResourceGPU: 5,
					},
				},
				Min: &framework.Resource{
					MilliCPU: 4000,
					Memory:   200,
				},
			},
			name:     "ElasticQuotaInfo OverMin Used Has GPU But Min Doesn't Have GPU",
			expected: true,
		},
		{
			before: &ElasticQuotaInfo{
				Namespace: "ns1",
				Used: &framework.Resource{
					MilliCPU:         300,
					Memory:           100,
					EphemeralStorage: 100,
				},
				Min: &framework.Resource{
					MilliCPU:         4000,
					Memory:           200,
					EphemeralStorage: 10,
				},
			},
			name:     "ElasticQuotaInfo OverMin EphemeralStorage",
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elasticQuotaInfo := tt.before
			actual := elasticQuotaInfo.usedOverMin()
			if actual != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, actual)
			}
		})
	}
}

func TestNewElasticQuotaInfo(t *testing.T) {
	type elasticQuotaParam struct {
		namespace string
		max       v1.ResourceList
		min       v1.ResourceList
		used      v1.ResourceList
	}

	tests := []struct {
		name              string
		elasticQuotaParam elasticQuotaParam
		expected          *ElasticQuotaInfo
	}{
		{
			name: "ElasticQuota",
			elasticQuotaParam: elasticQuotaParam{
				namespace: "ns1",
				max:       makeResourceList(100, 1000),
				min:       makeResourceList(10, 100),
				used:      makeResourceList(0, 0),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				pods:      sets.String{},
				Max: &framework.Resource{
					MilliCPU: 100,
					Memory:   1000,
				},
				Min: &framework.Resource{
					MilliCPU: 10,
					Memory:   100,
				},
				Used: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
			},
		},
		{
			name: "ElasticQuota Without Max",
			elasticQuotaParam: elasticQuotaParam{
				namespace: "ns1",
				max:       nil,
				min:       makeResourceList(10, 100),
				used:      makeResourceList(0, 0),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				pods:      sets.String{},
				Max: &framework.Resource{
					MilliCPU:         UpperBoundOfMax,
					Memory:           UpperBoundOfMax,
					EphemeralStorage: UpperBoundOfMax,
				},
				Min: &framework.Resource{
					MilliCPU: 10,
					Memory:   100,
				},
				Used: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
			},
		},
		{
			name: "ElasticQuota Without Min",
			elasticQuotaParam: elasticQuotaParam{
				namespace: "ns1",
				max:       makeResourceList(100, 1000),
				min:       nil,
				used:      makeResourceList(0, 0),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				pods:      sets.String{},
				Max: &framework.Resource{
					MilliCPU: 100,
					Memory:   1000,
				},
				Min: &framework.Resource{
					MilliCPU:         LowerBoundOfMin,
					Memory:           LowerBoundOfMin,
					EphemeralStorage: LowerBoundOfMin,
				},
				Used: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
			},
		},
		{
			name: "ElasticQuota Without Max and Min",
			elasticQuotaParam: elasticQuotaParam{
				namespace: "ns1",
				max:       nil,
				min:       nil,
				used:      makeResourceList(0, 0),
			},
			expected: &ElasticQuotaInfo{
				Namespace: "ns1",
				pods:      sets.String{},
				Max: &framework.Resource{
					MilliCPU:         UpperBoundOfMax,
					Memory:           UpperBoundOfMax,
					EphemeralStorage: UpperBoundOfMax,
				},
				Min: &framework.Resource{
					MilliCPU:         LowerBoundOfMin,
					Memory:           LowerBoundOfMin,
					EphemeralStorage: LowerBoundOfMin,
				},
				Used: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eqp := tt.elasticQuotaParam
			if got := newElasticQuotaInfo(eqp.namespace, eqp.min, eqp.max, eqp.used); !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}
