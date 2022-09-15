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

package v1beta2

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	schedulerconfigv1beta2 "k8s.io/kube-scheduler/config/v1beta2"
	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"

	config "sigs.k8s.io/scheduler-plugins/apis/config"
)

func TestConvert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(t *testing.T) {
	testCases := []struct {
		name        string
		in          *NodeResourceTopologyMatchArgs
		expected    *config.NodeResourceTopologyMatchArgs
		expectedErr error
	}{
		{
			name:     "empty NodeResourceTopologyMatchArgs",
			in:       &NodeResourceTopologyMatchArgs{},
			expected: &config.NodeResourceTopologyMatchArgs{},
		},
		{
			name: "type leastallocated, no resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: LeastAllocated,
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.LeastAllocated,
				},
			},
		},
		{
			name: "type balancedallocation, no resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: BalancedAllocation,
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.BalancedAllocation,
				},
			},
		},
		{
			name: "type mostallocated, no resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: MostAllocated,
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.MostAllocated,
				},
			},
		},
		{
			name: "type leastallocated, with resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: LeastAllocated,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "foo",
							Weight: 1234,
						},
					},
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.LeastAllocated,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "foo",
							Weight: 1234,
						},
					},
				},
			},
		},
		{
			name: "type balancedallocation, with resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: BalancedAllocation,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "cpu",
							Weight: 1111,
						},
						{
							Name:   "memory",
							Weight: 2222,
						},
					},
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.BalancedAllocation,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "cpu",
							Weight: 1111,
						},
						{
							Name:   "memory",
							Weight: 2222,
						},
					},
				},
			},
		},
		{
			name: "type mostallocated, with resources",
			in: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: MostAllocated,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "bar",
							Weight: 9999,
						},
					},
				},
			},
			expected: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.MostAllocated,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "bar",
							Weight: 9999,
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		got := &config.NodeResourceTopologyMatchArgs{}
		err := Convert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(tc.in, got, nil)
		if err != tc.expectedErr {
			t.Fatalf("inconsistent error got=%v expected=%v", err, tc.expectedErr)
		}
		if diff := cmp.Diff(got, tc.expected); diff != "" {
			t.Errorf("Got unexpected defaults (-want, +got):\n%s", diff)
		}
	}
}

func TestConvert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs(t *testing.T) {
	testCases := []struct {
		name        string
		in          *config.NodeResourceTopologyMatchArgs
		expected    *NodeResourceTopologyMatchArgs
		expectedErr error
	}{
		{
			name: "empty NodeResourceTopologyMatchArgs",
			in:   &config.NodeResourceTopologyMatchArgs{},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{},
			},
		},
		{
			name: "type leastallocated, no resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.LeastAllocated,
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: LeastAllocated,
				},
			},
		},
		{
			name: "type balancedallocation, no resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.BalancedAllocation,
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: BalancedAllocation,
				},
			},
		},
		{
			name: "type mostallocated, no resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.MostAllocated,
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: MostAllocated,
				},
			},
		},
		{
			name: "type leastallocated, with resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.LeastAllocated,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "foo",
							Weight: 1234,
						},
					},
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: LeastAllocated,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "foo",
							Weight: 1234,
						},
					},
				},
			},
		},
		{
			name: "type balancedallocation, with resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.BalancedAllocation,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "cpu",
							Weight: 1111,
						},
						{
							Name:   "memory",
							Weight: 2222,
						},
					},
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: BalancedAllocation,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "cpu",
							Weight: 1111,
						},
						{
							Name:   "memory",
							Weight: 2222,
						},
					},
				},
			},
		},
		{
			name: "type mostallocated, with resources",
			in: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.MostAllocated,
					Resources: []schedconfig.ResourceSpec{
						{
							Name:   "bar",
							Weight: 9999,
						},
					},
				},
			},
			expected: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type: MostAllocated,
					// random non-zero numbers
					Resources: []schedulerconfigv1beta2.ResourceSpec{
						{
							Name:   "bar",
							Weight: 9999,
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		got := &NodeResourceTopologyMatchArgs{}
		err := Convert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs(tc.in, got, nil)

		if got.ScoringStrategy == nil {
			t.Errorf("nil scoring strategy: should always be initialized")
		}

		if err != tc.expectedErr {
			t.Fatalf("inconsistent error got=%v expected=%v", err, tc.expectedErr)
		}
		if diff := cmp.Diff(got, tc.expected); diff != "" {
			t.Errorf("Got unexpected defaults (-want, +got):\n%s", diff)
		}
	}

}
