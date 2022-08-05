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
