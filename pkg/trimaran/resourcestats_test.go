/*
Copyright 2022 The Kubernetes Authors.

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

package trimaran

import (
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

var (
	metrics = []watcher.Metric{
		{
			Name:     "no_name",
			Type:     watcher.CPU,
			Operator: "",
			Value:    40,
		},
		{
			Name:     "cpu_running_avg",
			Type:     watcher.CPU,
			Operator: watcher.Average,
			Value:    40,
		},
		{
			Name:     "cpu_running_std",
			Type:     watcher.CPU,
			Operator: watcher.Std,
			Value:    36,
		},
		{
			Name:     "mem_running_avg",
			Type:     watcher.Memory,
			Operator: watcher.Average,
			Value:    20,
		},
		{
			Name:     "mem_running_std",
			Type:     watcher.Memory,
			Operator: watcher.Std,
			Value:    10,
		},
	}

	nodeResources = map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

	node0 = st.MakeNode().Name("node0").Capacity(nodeResources).Obj()
)

func TestCreateResourceStats(t *testing.T) {
	pr := &framework.Resource{
		MilliCPU: 100,
		Memory:   1024 * 1024,
	}

	rsExpectedCPU := &ResourceStats{
		Capacity:  1000,
		Req:       100,
		UsedAvg:   400,
		UsedStdev: 360,
	}

	rsExpectedMem := &ResourceStats{
		Capacity:  1024,
		Req:       1,
		UsedAvg:   204.8,
		UsedStdev: 102.4,
	}

	type args struct {
		metrics      []watcher.Metric
		node         *v1.Node
		podRequest   *framework.Resource
		resourceName v1.ResourceName
		watcherType  string
	}
	tests := []struct {
		name        string
		args        args
		wantRs      *ResourceStats
		wantIsValid bool
	}{
		{
			name: "test-cpu",
			args: args{
				metrics:      metrics,
				node:         node0,
				podRequest:   pr,
				resourceName: v1.ResourceCPU,
				watcherType:  watcher.CPU,
			},
			wantRs:      rsExpectedCPU,
			wantIsValid: true,
		},
		{
			name: "test-missing",
			args: args{
				metrics: []watcher.Metric{
					metrics[3],
					metrics[4],
				},
				node:         node0,
				podRequest:   pr,
				resourceName: v1.ResourceCPU,
				watcherType:  watcher.CPU,
			},
			wantRs:      nil,
			wantIsValid: false,
		},
		{
			name: "test-memory",
			args: args{
				metrics:      metrics,
				node:         node0,
				podRequest:   pr,
				resourceName: v1.ResourceMemory,
				watcherType:  watcher.Memory,
			},
			wantRs:      rsExpectedMem,
			wantIsValid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRs, gotIsValid := CreateResourceStats(tt.args.metrics, tt.args.node, tt.args.podRequest, tt.args.resourceName, tt.args.watcherType)
			if !reflect.DeepEqual(gotRs, tt.wantRs) {
				t.Errorf("createResourceStats() gotRs = %v, want %v", gotRs, tt.wantRs)
			}
			if gotIsValid != tt.wantIsValid {
				t.Errorf("createResourceStats() gotIsValid = %v, want %v", gotIsValid, tt.wantIsValid)
			}
		})
	}
}
func TestGetResourceRequested(t *testing.T) {
	var ovhd int64 = 10
	var initCPUReq int64 = 100
	var initMemReq int64 = 512
	contCPUReq := []int64{1000, 500}
	contMemReq := []int64{2048, 1024}

	pod := getPodWithContainersAndOverhead(ovhd, initCPUReq, initMemReq, contCPUReq, contMemReq)
	res := GetResourceRequested(pod)
	resExpected := &framework.Resource{
		MilliCPU: 1510,
		Memory:   3072,
	}
	assert.EqualValues(t, resExpected, res)

	initCPUReq = 2000
	initMemReq = 4096
	pod = getPodWithContainersAndOverhead(ovhd, initCPUReq, initMemReq, contCPUReq, contMemReq)
	res = GetResourceRequested(pod)
	resExpected = &framework.Resource{
		MilliCPU: 2010,
		Memory:   4096,
	}
	assert.EqualValues(t, resExpected, res)

	ovhd = 0
	initCPUReq = 0
	initMemReq = 0
	contCPUReq = []int64{0, 0, 1000}
	contMemReq = []int64{0, 200 * 1024 * 1024, 400 * 1024 * 1024}

	pod = getPodWithContainersAndOverhead(ovhd, initCPUReq, initMemReq, contCPUReq, contMemReq)
	res = GetResourceRequested(pod)
	resExpected = &framework.Resource{
		MilliCPU: 1000,
		Memory:   600 * 1024 * 1024,
	}
	assert.EqualValues(t, resExpected, res)
}

func TestGetResourceLimits(t *testing.T) {
	var ovhd int64 = 10
	var initCPUReq int64 = 100
	var initMemReq int64 = 512
	contCPUReq := []int64{1000, 500}
	contMemReq := []int64{2048, 1024}

	var initCPULimit int64 = 2000
	var initMemLimit int64 = 4096
	contCPULimit := []int64{1000, 500}
	contMemLimit := []int64{2048, 1024}

	pod := getPodWithContainersAndOverhead(ovhd, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod = getPodWithLimits(pod, initCPULimit, initMemLimit, contCPULimit, contMemLimit)

	res := GetResourceLimits(pod)
	resExpected := &framework.Resource{
		MilliCPU: 2010,
		Memory:   4096,
	}
	assert.EqualValues(t, resExpected, res)

	contCPULimitShort := []int64{4000}
	pod = getPodWithLimits(pod, initCPULimit, initMemLimit, contCPULimitShort, contMemLimit)
	res = GetResourceLimits(pod)
	resExpected = &framework.Resource{
		MilliCPU: 2010,
		Memory:   4096,
	}
	assert.EqualValues(t, resExpected, res)

	contCPULimit = []int64{1000, 2500}
	contMemLimit = []int64{4096, 2048}
	pod = getPodWithLimits(pod, initCPULimit, initMemLimit, contCPULimit, contMemLimit)
	res = GetResourceLimits(pod)
	resExpected = &framework.Resource{
		MilliCPU: 3510,
		Memory:   6144,
	}
	assert.EqualValues(t, resExpected, res)

	var ovhd0 int64 = 0
	contCPUReq0 := []int64{1000, 500, 0}
	contMemReq0 := []int64{2048, 1024, 0}
	contCPULimit0 := []int64{0, 0, 1000}
	contMemLimit0 := []int64{0, 2048, 0}
	pod0 := getPodWithContainersAndOverhead(ovhd0, initCPUReq, initMemReq, contCPUReq0, contMemReq0)
	pod0 = getPodWithLimits(pod0, initCPULimit, initMemLimit, contCPULimit0, contMemLimit0)
	res0 := GetResourceLimits(pod0)
	resExpected = &framework.Resource{
		MilliCPU: 2000,
		Memory:   4096,
	}
	assert.EqualValues(t, resExpected, res0)
}

func TestGetMuSigma(t *testing.T) {
	type args struct {
		rs *ResourceStats
	}
	tests := []struct {
		name      string
		args      args
		wantMu    float64
		wantSigma float64
	}{
		{
			name: "proper arguments",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       100,
					UsedAvg:   400,
					UsedStdev: 360,
				},
			},
			wantMu:    0.5,
			wantSigma: 0.36,
		},
		{
			name: "zero arguments",
			args: args{
				rs: &ResourceStats{
					Capacity:  0,
					Req:       0,
					UsedAvg:   0,
					UsedStdev: 0,
				},
			},
			wantMu:    0,
			wantSigma: 0,
		},
		{
			name: "large used",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       100,
					UsedAvg:   1400,
					UsedStdev: 300,
				},
			},
			wantMu:    1.0,
			wantSigma: 0.3,
		},
		{
			name: "large deviation",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       100,
					UsedAvg:   400,
					UsedStdev: 1600,
				},
			},
			wantMu:    0.5,
			wantSigma: 1.0,
		},
		{
			name: "large arguments",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       0,
					UsedAvg:   1400,
					UsedStdev: 1600,
				},
			},
			wantMu:    1.0,
			wantSigma: 1.0,
		},
		{
			name: "negative used",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       0,
					UsedAvg:   -100,
					UsedStdev: 200,
				},
			},
			wantMu:    0.0,
			wantSigma: 0.2,
		},
		{
			name: "negative deviation",
			args: args{
				rs: &ResourceStats{
					Capacity:  1000,
					Req:       400,
					UsedAvg:   0,
					UsedStdev: -200,
				},
			},
			wantMu:    0.4,
			wantSigma: 0.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu, sigma := GetMuSigma(tt.args.rs)
			if mu != tt.wantMu {
				t.Errorf("GetMuSigma(): mu got = %v, want %v", mu, tt.wantMu)
			}
			if sigma != tt.wantSigma {
				t.Errorf("GetMuSigma(): sigma got = %v, want %v", sigma, tt.wantSigma)
			}
		})
	}
}

func TestGetNodeRequestsAndLimits(t *testing.T) {
	nr := map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "6Ki",
	}

	testNode := st.MakeNode().Name("test-node").Capacity(nr).Obj()

	nr = map[v1.ResourceName]string{
		v1.ResourceCPU:    "1600m",
		v1.ResourceMemory: "6Ki",
	}

	testNodeWithLowNodeCapacity := st.MakeNode().Name("test-node").Capacity(nr).Obj()

	var initCPUReq int64 = 100
	var initMemReq int64 = 2048
	contCPUReq := []int64{1000, 500}
	contMemReq := []int64{512, 1024}

	var initCPULimit int64 = 500
	var initMemLimit int64 = 2048
	contCPULimit := []int64{1500, 500}
	contMemLimit := []int64{1024, 1024}

	pod := getPodWithContainersAndOverhead(0, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod = getPodWithLimits(pod, initCPULimit, initMemLimit, contCPULimit, contMemLimit)

	pod1 := getPodWithContainersAndOverhead(0, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod1 = getPodWithLimits(pod1, initCPULimit, initMemLimit, contCPULimit, contMemLimit)
	podInfo1, _ := framework.NewPodInfo(pod1)

	pod2 := getPodWithContainersAndOverhead(0, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod2 = getPodWithLimits(pod2, initCPULimit, initMemLimit, contCPULimit, contMemLimit)
	podInfo2, _ := framework.NewPodInfo(pod2)

	contCPULimit = []int64{1000, 1000}
	contMemLimit = []int64{1024, 2048}

	pod3 := getPodWithContainersAndOverhead(0, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod3 = getPodWithLimits(pod3, initCPULimit, initMemLimit, contCPULimit, contMemLimit)
	podInfo3, _ := framework.NewPodInfo(pod3)

	podRequests := GetResourceRequested(pod)
	podLimits := GetResourceLimits(pod)

	allocatableResources := testNodeWithLowNodeCapacity.Status.Allocatable
	amCpu := allocatableResources[v1.ResourceCPU]
	capCpu := amCpu.MilliValue()
	amMem := allocatableResources[v1.ResourceMemory]
	capMem := amMem.Value()

	contCPULimit = []int64{1000, 400}

	pod4 := getPodWithContainersAndOverhead(0, initCPUReq, initMemReq, contCPUReq, contMemReq)
	pod4 = getPodWithLimits(pod4, initCPULimit, initMemLimit, contCPULimit, contMemLimit)

	pod4Requests := GetResourceRequested(pod4)
	pod4Limits := GetResourceLimits(pod4)

	type args struct {
		podsOnNode  []*framework.PodInfo
		node        *v1.Node
		pod         *v1.Pod
		podRequests *framework.Resource
		podLimits   *framework.Resource
	}
	tests := []struct {
		name string
		args args
		want *NodeRequestsAndLimits
	}{
		{
			name: "test-0",
			args: args{
				podsOnNode: []*framework.PodInfo{
					podInfo1,
					podInfo2},
				node:        testNode,
				pod:         pod,
				podRequests: podRequests,
				podLimits:   podLimits,
			},
			want: &NodeRequestsAndLimits{
				NodeRequest: &framework.Resource{
					MilliCPU: 3 * 1500,
					Memory:   3 * 2048,
				},
				NodeLimit: &framework.Resource{
					MilliCPU: 3 * 2000,
					Memory:   3 * 2048,
				},
				NodeRequestMinusPod: &framework.Resource{
					MilliCPU: 2 * 1500,
					Memory:   2 * 2048,
				},
				NodeLimitMinusPod: &framework.Resource{
					MilliCPU: 2 * 2000,
					Memory:   2 * 2048,
				},
				Nodecapacity: &framework.Resource{
					MilliCPU: 8000,
					Memory:   6 * 1024,
				},
			},
		},
		{
			name: "test-1",
			args: args{
				podsOnNode: []*framework.PodInfo{
					podInfo3},
				node:        testNode,
				pod:         pod,
				podRequests: podRequests,
				podLimits:   podLimits,
			},
			want: &NodeRequestsAndLimits{
				NodeRequest: &framework.Resource{
					MilliCPU: 2 * 1500,
					Memory:   2 * 2048,
				},
				NodeLimit: &framework.Resource{
					MilliCPU: 2 * 2000,
					Memory:   2*2048 + 1024,
				},
				NodeRequestMinusPod: &framework.Resource{
					MilliCPU: 1500,
					Memory:   2048,
				},
				NodeLimitMinusPod: &framework.Resource{
					MilliCPU: 2000,
					Memory:   2048 + 1024,
				},
				Nodecapacity: &framework.Resource{
					MilliCPU: 8000,
					Memory:   6 * 1024,
				},
			},
		},
		{
			name: "test-2",
			// Test case for Node with low capacity than the requested Pod limits
			args: args{
				podsOnNode:  []*framework.PodInfo{},
				node:        testNodeWithLowNodeCapacity,
				pod:         pod,
				podRequests: podRequests,
				podLimits:   podLimits,
			},
			want: &NodeRequestsAndLimits{
				NodeRequest: &framework.Resource{
					MilliCPU: int64(math.Min(float64(GetResourceRequested(pod).MilliCPU), float64(capCpu))),
					Memory:   int64(math.Min(float64(GetResourceRequested(pod).Memory), float64(capMem))),
				},
				NodeLimit: &framework.Resource{
					MilliCPU: int64(math.Max(float64(GetResourceRequested(pod).MilliCPU), float64(GetResourceLimits(pod).MilliCPU))),
					Memory:   int64(math.Max(float64(GetResourceRequested(pod).Memory), float64(GetResourceLimits(pod).Memory))),
				},
				NodeRequestMinusPod: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
				NodeLimitMinusPod: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
				Nodecapacity: &framework.Resource{
					MilliCPU: capCpu,
					Memory:   capMem,
				},
			},
		},
		{
			name: "test-3",
			// Test case for Pod with more Requests than Limits
			args: args{
				podsOnNode:  []*framework.PodInfo{},
				node:        testNodeWithLowNodeCapacity,
				pod:         pod4,
				podRequests: pod4Requests,
				podLimits:   pod4Limits,
			},
			want: &NodeRequestsAndLimits{
				NodeRequest: &framework.Resource{
					MilliCPU: int64(math.Min(float64(GetResourceRequested(pod4).MilliCPU), float64(capCpu))),
					Memory:   int64(math.Min(float64(GetResourceRequested(pod4).Memory), float64(capMem))),
				},
				NodeLimit: &framework.Resource{
					MilliCPU: int64(math.Min(
						math.Max(float64(GetResourceRequested(pod4).MilliCPU), float64(GetResourceLimits(pod4).MilliCPU)),
						float64(capCpu))),
					Memory: int64(math.Min(
						math.Max(float64(GetResourceRequested(pod4).Memory), float64(GetResourceLimits(pod4).Memory)),
						float64(capMem))),
				},
				NodeRequestMinusPod: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
				NodeLimitMinusPod: &framework.Resource{
					MilliCPU: 0,
					Memory:   0,
				},
				Nodecapacity: &framework.Resource{
					MilliCPU: capCpu,
					Memory:   capMem,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *NodeRequestsAndLimits
			SetMaxLimits(tt.args.podRequests, tt.args.podLimits)
			if got = GetNodeRequestsAndLimits(tt.args.podsOnNode, tt.args.node, tt.args.pod,
				tt.args.podRequests, tt.args.podLimits); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetNodeRequestsAndLimits(): got = {%+v, %+v, %+v, %+v, %+v}, want = {%+v, %+v, %+v, %+v, %+v}",
					*got.NodeRequest, *got.NodeLimit, *got.NodeRequestMinusPod, *got.NodeLimitMinusPod, *got.Nodecapacity,
					*tt.want.NodeRequest, *tt.want.NodeLimit, *tt.want.NodeRequestMinusPod, *tt.want.NodeLimitMinusPod, *tt.want.Nodecapacity)
			}
			// The below asserts should hold good for all test cases, ideally; "test-3" exercises this by setting NodeLimit less than NodeRequest
			if tt.name == "test-3" {
				assert.LessOrEqual(t, (*got.NodeRequest).MilliCPU, (*got.NodeLimit).MilliCPU)
				assert.LessOrEqual(t, (*got.NodeRequest).Memory, (*got.NodeLimit).Memory)
			}
		})
	}
}

// getPodWithContainersAndOverhead : length of contCPUReq and contMemReq should be same
func getPodWithContainersAndOverhead(overhead int64, initCPUReq int64, initMemReq int64, contCPUReq []int64, contMemReq []int64) *v1.Pod {
	newPod := st.MakePod()
	newPod.Spec.Overhead = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.Overhead[v1.ResourceCPU] = *resource.NewMilliQuantity(overhead, resource.DecimalSI)

	newPod.Spec.InitContainers = []v1.Container{
		{Name: "test-init"},
	}
	newPod.Spec.InitContainers[0].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.InitContainers[0].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(initCPUReq, resource.DecimalSI)
	newPod.Spec.InitContainers[0].Resources.Requests[v1.ResourceMemory] = *resource.NewQuantity(initMemReq, resource.DecimalSI)

	for i := 0; i < len(contCPUReq); i++ {
		newPod.Container("test-container-" + strconv.Itoa(i))
	}
	for i, request := range contCPUReq {
		newPod.Spec.Containers[i].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceMemory] = *resource.NewQuantity(contMemReq[i], resource.DecimalSI)

		newPod.Spec.Containers[i].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceMemory] = *resource.NewQuantity(contMemReq[i], resource.DecimalSI)
	}
	return newPod.Obj()
}

// getPodWithLimits : length of contCPULimit and contMemLimit should be same as number on containers
func getPodWithLimits(pod *v1.Pod, initCPULimit int64, initMemLimit int64, contCPULimit []int64, contMemLimit []int64) *v1.Pod {
	if len(contCPULimit) != len(contMemLimit) || len(contCPULimit) != len(pod.Spec.Containers) {
		return pod
	}

	pod.Spec.InitContainers[0].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
	pod.Spec.InitContainers[0].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(initCPULimit, resource.DecimalSI)
	pod.Spec.InitContainers[0].Resources.Limits[v1.ResourceMemory] = *resource.NewQuantity(initMemLimit, resource.DecimalSI)

	for i := 0; i < len(pod.Spec.Containers); i++ {
		pod.Spec.Containers[i].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
		pod.Spec.Containers[i].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(contCPULimit[i], resource.DecimalSI)
		pod.Spec.Containers[i].Resources.Limits[v1.ResourceMemory] = *resource.NewQuantity(contMemLimit[i], resource.DecimalSI)
	}
	return pod
}
