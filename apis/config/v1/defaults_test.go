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

package v1

import (
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	schedulerconfigv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/utils/pointer"
)

func TestSchedulingDefaults(t *testing.T) {
	tests := []struct {
		name   string
		config runtime.Object
		expect runtime.Object
	}{
		{
			name:   "empty config CoschedulingArgs",
			config: &CoschedulingArgs{},
			expect: &CoschedulingArgs{
				PermitWaitingTimeSeconds: pointer.Int64Ptr(60),
				PodGroupBackoffSeconds:   pointer.Int64Ptr(0),
			},
		},
		{
			name: "set non default CoschedulingArgs",
			config: &CoschedulingArgs{
				PermitWaitingTimeSeconds: pointer.Int64Ptr(60),
				PodGroupBackoffSeconds:   pointer.Int64Ptr(20),
			},
			expect: &CoschedulingArgs{
				PermitWaitingTimeSeconds: pointer.Int64Ptr(60),
				PodGroupBackoffSeconds:   pointer.Int64Ptr(20),
			},
		},
		{
			name:   "empty config NodeResourcesAllocatableArgs",
			config: &NodeResourcesAllocatableArgs{},
			expect: &NodeResourcesAllocatableArgs{
				Resources: []schedulerconfigv1.ResourceSpec{
					{Name: "cpu", Weight: 1 << 20}, {Name: "memory", Weight: 1},
				},
				Mode: Least,
			},
		},
		{
			name: "set non default NodeResourcesAllocatableArgs",
			config: &NodeResourcesAllocatableArgs{
				Resources: []schedulerconfigv1.ResourceSpec{
					{Name: "cpu", Weight: 1 << 10}, {Name: "memory", Weight: 2},
				},
				Mode: Most,
			},
			expect: &NodeResourcesAllocatableArgs{
				Resources: []schedulerconfigv1.ResourceSpec{
					{Name: "cpu", Weight: 1 << 10}, {Name: "memory", Weight: 2},
				},
				Mode: Most,
			},
		},
		{
			name:   "empty config TargetLoadPackingArgs",
			config: &TargetLoadPackingArgs{},
			expect: &TargetLoadPackingArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				DefaultRequests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(
					strconv.FormatInt(DefaultRequestsMilliCores, 10) + "m")},
				DefaultRequestsMultiplier: pointer.StringPtr("1.5"),
				TargetUtilization:         pointer.Int64Ptr(40),
			},
		},
		{
			name: "set non default TargetLoadPackingArgs",
			config: &TargetLoadPackingArgs{
				TrimaranSpec: TrimaranSpec{
					WatcherAddress: pointer.StringPtr("http://localhost:2020")},
				DefaultRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
				DefaultRequestsMultiplier: pointer.StringPtr("2.5"),
				TargetUtilization:         pointer.Int64Ptr(50),
			},
			expect: &TargetLoadPackingArgs{
				TrimaranSpec: TrimaranSpec{
					WatcherAddress: pointer.StringPtr("http://localhost:2020")},
				DefaultRequests:           v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
				DefaultRequestsMultiplier: pointer.StringPtr("2.5"),
				TargetUtilization:         pointer.Int64Ptr(50),
			},
		},
		{
			name:   "empty config LoadVariationRiskBalancingArgs",
			config: &LoadVariationRiskBalancingArgs{},
			expect: &LoadVariationRiskBalancingArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				SafeVarianceMargin:      pointer.Float64Ptr(1.0),
				SafeVarianceSensitivity: pointer.Float64Ptr(1.0),
			},
		},
		{
			name: "set non default LoadVariationRiskBalancingArgs",
			config: &LoadVariationRiskBalancingArgs{
				SafeVarianceMargin:      pointer.Float64Ptr(2.0),
				SafeVarianceSensitivity: pointer.Float64Ptr(2.0),
			},
			expect: &LoadVariationRiskBalancingArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				SafeVarianceMargin:      pointer.Float64Ptr(2.0),
				SafeVarianceSensitivity: pointer.Float64Ptr(2.0),
			},
		},
		{
			name:   "empty config LowRiskOverCommitmentArgs",
			config: &LowRiskOverCommitmentArgs{},
			expect: &LowRiskOverCommitmentArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				SmoothingWindowSize: pointer.Int64Ptr(5),
				RiskLimitWeights: map[v1.ResourceName]float64{
					v1.ResourceCPU:    0.5,
					v1.ResourceMemory: 0.5,
				},
			},
		},
		{
			name: "set non default LowRiskOverCommitmentArgs",
			config: &LowRiskOverCommitmentArgs{
				SmoothingWindowSize: pointer.Int64Ptr(10),
				RiskLimitWeights: map[v1.ResourceName]float64{
					v1.ResourceCPU:    0.2,
					v1.ResourceMemory: 0.8,
				},
			},
			expect: &LowRiskOverCommitmentArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				SmoothingWindowSize: pointer.Int64Ptr(10),
				RiskLimitWeights: map[v1.ResourceName]float64{
					v1.ResourceCPU:    0.2,
					v1.ResourceMemory: 0.8,
				},
			},
		},
		{
			name: "set out of range LowRiskOverCommitmentArgs",
			config: &LowRiskOverCommitmentArgs{
				SmoothingWindowSize: pointer.Int64Ptr(10),
				RiskLimitWeights: map[v1.ResourceName]float64{
					v1.ResourceCPU:    -1,
					v1.ResourceMemory: 2,
				},
			},
			expect: &LowRiskOverCommitmentArgs{
				TrimaranSpec: TrimaranSpec{
					MetricProvider: MetricProviderSpec{
						Type: "KubernetesMetricsServer",
					}},
				SmoothingWindowSize: pointer.Int64Ptr(10),
				RiskLimitWeights: map[v1.ResourceName]float64{
					v1.ResourceCPU:    0.5,
					v1.ResourceMemory: 0.5,
				},
			},
		},
		{
			name:   "empty config NodeResourceTopologyMatchArgs",
			config: &NodeResourceTopologyMatchArgs{},
			expect: &NodeResourceTopologyMatchArgs{
				ScoringStrategy: &ScoringStrategy{
					Type:      LeastAllocated,
					Resources: defaultResourceSpec,
				},
				Cache: &NodeResourceTopologyCache{
					ForeignPodsDetect: &defaultForeignPodsDetect,
					ResyncMethod:      &defaultResyncMethod,
				},
			},
		},
		{
			name:   "empty config PreeemptionTolerationArgs",
			config: &PreemptionTolerationArgs{},
			expect: &PreemptionTolerationArgs{
				MinCandidateNodesPercentage: pointer.Int32Ptr(10),
				MinCandidateNodesAbsolute:   pointer.Int32Ptr(100),
			},
		},
		{
			name:   "empty config TopologySortArgs",
			config: &TopologicalSortArgs{},
			expect: &TopologicalSortArgs{
				Namespaces: []string{"default"},
			},
		},
		{
			name: "set non default TopologySortArgs",
			config: &TopologicalSortArgs{
				Namespaces: []string{"n1"},
			},
			expect: &TopologicalSortArgs{
				Namespaces: []string{"n1"},
			},
		},
		{
			name:   "empty config NetworkOverheadArgs",
			config: &NetworkOverheadArgs{},
			expect: &NetworkOverheadArgs{
				Namespaces:          []string{"default"},
				WeightsName:         pointer.StringPtr("UserDefined"),
				NetworkTopologyName: pointer.StringPtr("nt-default"),
			},
		},
		{
			name: "set non default TopologySortArgs",
			config: &NetworkOverheadArgs{
				Namespaces:          []string{"n2"},
				WeightsName:         pointer.StringPtr("latency"),
				NetworkTopologyName: pointer.StringPtr("nt-latency-costs"),
			},
			expect: &NetworkOverheadArgs{
				Namespaces:          []string{"n2"},
				WeightsName:         pointer.StringPtr("latency"),
				NetworkTopologyName: pointer.StringPtr("nt-latency-costs"),
			},
		},
	}

	for _, tc := range tests {
		scheme := runtime.NewScheme()
		utilruntime.Must(AddToScheme(scheme))
		t.Run(tc.name, func(t *testing.T) {
			scheme.Default(tc.config)
			if diff := cmp.Diff(tc.config, tc.expect); diff != "" {
				t.Errorf("Got unexpected defaults (-want, +got):\n%s", diff)
			}
		})
	}
}
