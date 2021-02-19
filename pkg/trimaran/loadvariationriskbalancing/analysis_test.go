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
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"

	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
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
		name     string
		margin   float64
		rs       *resourceStats
		expected int64
		response int64
	}{
		{
			name:   "valid data",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 45,
		},
		{
			name:   "zero capacity",
			margin: 1,
			rs: &resourceStats{
				capacity:  0,
				demand:    10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 0,
		},
		{
			name:   "negative usedAvg",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   -40,
				usedStdev: 36,
			},
			expected: 65,
		},
		{
			name:   "large usedAvg",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   200,
				usedStdev: 36,
			},
			expected: 20,
		},
		{
			name:   "negative usedStdev",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   40,
				usedStdev: -36,
			},
			expected: 75,
		},
		{
			name:   "large usedStdev",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   40,
				usedStdev: 120,
			},
			expected: 25,
		},
		{
			name:   "large usedAvg",
			margin: 1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   200,
				usedStdev: 36,
			},
			expected: 20,
		},
		{
			name:   "negative margin",
			margin: -1,
			rs: &resourceStats{
				capacity:  100,
				demand:    10,
				usedAvg:   40,
				usedStdev: 36,
			},
			expected: 75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := tt.rs.computeScore(tt.margin)
			assert.Equal(t, tt.expected, response)
		})
	}
}

func TestGetCPUStats(t *testing.T) {
	pr := &framework.Resource{
		MilliCPU: 100,
		Memory:   1000,
	}

	rsExpected := &resourceStats{
		capacity:  1000,
		demand:    100,
		usedAvg:   400,
		usedStdev: 360,
	}

	rs, ok := getCPUStats(metrics, node0, pr)
	assert.True(t, ok)
	assert.EqualValues(t, rsExpected, rs)

	missingMetrics := []watcher.Metric{
		metrics[3],
		metrics[4],
	}
	_, ok = getCPUStats(missingMetrics, node0, pr)
	assert.False(t, ok)
}

func TestGetMemoryStats(t *testing.T) {
	pr := &framework.Resource{
		MilliCPU: 100,
		Memory:   1024 * 1024,
	}

	rsExpected := &resourceStats{
		capacity:  1024,
		demand:    1,
		usedAvg:   204.8,
		usedStdev: 102.4,
	}

	rs, ok := getMemoryStats(metrics, node0, pr)
	assert.True(t, ok)
	assert.EqualValues(t, rsExpected, rs)

	missingMetrics := []watcher.Metric{
		metrics[1],
		metrics[2],
	}
	_, ok = getMemoryStats(missingMetrics, node0, pr)
	assert.False(t, ok)
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
