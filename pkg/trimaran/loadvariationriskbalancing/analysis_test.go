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

package loadvariationriskbalancing

import (
	"math"
	"reflect"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
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

func TestComputeScore(t *testing.T) {
	tests := []struct {
		name        string
		margin      float64
		sensitivity float64
		rs          *resourceStats
		expected    int64
	}{
		{
			name:        "valid data",
			margin:      1,
			sensitivity: 1,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 57,
		},
		{
			name:        "zero capacity",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  0,
				req:       10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 0,
		},
		{
			name:        "negative usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   -40,
				usedStdev: 36,
			},
			expected: 65,
		},
		{
			name:        "large usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   200,
				usedStdev: 36,
			},
			expected: 20,
		},
		{
			name:        "negative usedStdev",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: -36,
			},
			expected: 75,
		},
		{
			name:        "large usedStdev",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: 120,
			},
			expected: 25,
		},
		{
			name:        "large usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   200,
				usedStdev: 36,
			},
			expected: 20,
		},
		{
			name:        "negative margin",
			margin:      -1,
			sensitivity: 1,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 75,
		},
		{
			name:        "negative sensitivity",
			margin:      1,
			sensitivity: -1,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 57,
		},
		{
			name:        "zero sensitivity",
			margin:      1,
			sensitivity: 0,
			rs: &resourceStats{
				capacity:  100,
				req:       10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := int64(math.Round(tt.rs.computeScore(tt.margin, tt.sensitivity)))
			assert.Equal(t, tt.expected, response)
		})
	}
}

func Test_createResourceStats(t *testing.T) {
	pr := &framework.Resource{
		MilliCPU: 100,
		Memory:   1024 * 1024,
	}

	rsExpectedCPU := &resourceStats{
		capacity:  1000,
		req:       100,
		usedAvg:   400,
		usedStdev: 360,
	}

	rsExpectedMem := &resourceStats{
		capacity:  1024,
		req:       1,
		usedAvg:   204.8,
		usedStdev: 102.4,
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
		wantRs      *resourceStats
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
			gotRs, gotIsValid := createResourceStats(tt.args.metrics, tt.args.node, tt.args.podRequest, tt.args.resourceName, tt.args.watcherType)
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
	res := getResourceRequested(pod)
	resExpected := &framework.Resource{
		MilliCPU: 1510,
		Memory:   3072,
	}
	assert.EqualValues(t, resExpected, res)

	initCPUReq = 2000
	initMemReq = 4096
	pod = getPodWithContainersAndOverhead(ovhd, initCPUReq, initMemReq, contCPUReq, contMemReq)
	res = getResourceRequested(pod)
	resExpected = &framework.Resource{
		MilliCPU: 2010,
		Memory:   4096,
	}
	assert.EqualValues(t, resExpected, res)
}
