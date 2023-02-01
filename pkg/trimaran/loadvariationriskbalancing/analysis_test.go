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
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

func TestComputeScore(t *testing.T) {
	tests := []struct {
		name        string
		margin      float64
		sensitivity float64
		rs          *trimaran.ResourceStats
		expected    int64
	}{
		{
			name:        "valid data",
			margin:      1,
			sensitivity: 1,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 36,
			},
			expected: 57,
		},
		{
			name:        "zero capacity",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  0,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 36,
			},
			expected: 0,
		},
		{
			name:        "negative usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   -40,
				UsedStdev: 36,
			},
			expected: 65,
		},
		{
			name:        "large usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   200,
				UsedStdev: 36,
			},
			expected: 20,
		},
		{
			name:        "negative usedStdev",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: -36,
			},
			expected: 75,
		},
		{
			name:        "large usedStdev",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 120,
			},
			expected: 25,
		},
		{
			name:        "large usedAvg",
			margin:      1,
			sensitivity: 2,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   200,
				UsedStdev: 36,
			},
			expected: 20,
		},
		{
			name:        "negative margin",
			margin:      -1,
			sensitivity: 1,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 36,
			},
			expected: 75,
		},
		{
			name:        "negative sensitivity",
			margin:      1,
			sensitivity: -1,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 36,
			},
			expected: 57,
		},
		{
			name:        "zero sensitivity",
			margin:      1,
			sensitivity: 0,
			rs: &trimaran.ResourceStats{
				Capacity:  100,
				Req:       10,
				UsedAvg:   40,
				UsedStdev: 36,
			},
			expected: 75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := int64(math.Round(computeScore(tt.rs, tt.margin, tt.sensitivity)))
			assert.Equal(t, tt.expected, response)
		})
	}
}
