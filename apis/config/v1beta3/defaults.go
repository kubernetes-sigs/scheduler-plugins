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

package v1beta3

import (
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	schedulerconfigv1beta3 "k8s.io/kube-scheduler/config/v1beta3"
	k8sschedulerconfigv1beta3 "k8s.io/kubernetes/pkg/scheduler/apis/config/v1beta3"
)

var (
	defaultPermitWaitingTimeSeconds     int64 = 60
	defaultPodGroupBackoffSeconds       int64 = 0
	defaultNodeResourcesAllocatableMode       = Least

	// defaultResourcesToWeightMap is used to set the default resourceToWeight map for CPU and memory
	// used by the NodeResourcesAllocatable scoring plugin.
	// The base unit for CPU is millicore, while the base using for memory is a byte.
	// The default CPU weight is 1<<20 and default memory weight is 1. That means a millicore
	// has a weighted score equivalent to 1 MiB.
	defaultNodeResourcesAllocatableResourcesToWeightMap = []schedulerconfigv1beta3.ResourceSpec{
		{Name: "cpu", Weight: 1 << 20}, {Name: "memory", Weight: 1},
	}

	// Defaults for TargetLoadPacking plugin

	// Default 1 core CPU usage for containers without requests and limits i.e. Best Effort QoS.
	DefaultRequestsMilliCores int64 = 1000
	// DefaultRequestsMultiplier for containers without limits predicted as 1.5*requests i.e. Burstable QoS class
	DefaultRequestsMultiplier = "1.5"
	// DefaultTargetUtilizationPercent Recommended to keep -10 than desired limit.
	DefaultTargetUtilizationPercent int64 = 40

	// Defaults for LoadVariationRiskBalancing plugin

	// Risk is usually calculated as average (aka. mu) plus standard deviation (aka. sigma).
	// In order to allow customization in the calculation of risk, two parameters are provided:
	// Margin and Sensitivity. Margin is a multiplier of sigma, and Sensitivity is a root power of sigma.
	// For example, Margin=3 and Sensitivity=2 leads to a risk evaluated as: mu + 3 sqrt(sigma).
	// The default value for both parameters is 1, leading to: mu + sigma.
	// DefaultSafeVarianceMargin is one
	DefaultSafeVarianceMargin = 1.0
	// DefaultSafeVarianceSensitivity is one
	DefaultSafeVarianceSensitivity = 1.0

	// Defaults for LowRiskOverCommitment plugin

	// The default number of windows over which usage data metrics are smoothed.
	// DefaultSmoothingWindowSize is 5 (used by Prometheus)
	DefaultSmoothingWindowSize int64 = 5
	// The default weight of risk due to limit for a resource
	DefaultRiskLimitWeight float64 = 0.5
	// Resources fractional weight of risk due to limits specification [0,1]
	DefaultRiskLimitWeights = map[v1.ResourceName]float64{
		v1.ResourceCPU:    DefaultRiskLimitWeight,
		v1.ResourceMemory: DefaultRiskLimitWeight,
	}

	// DefaultMetricProviderType is the Kubernetes metrics server
	DefaultMetricProviderType = KubernetesMetricsServer
	// DefaultInsecureSkipVerify is whether to skip the certificate verification
	DefaultInsecureSkipVerify = true

	defaultResourceSpec = []schedulerconfigv1beta3.ResourceSpec{
		{Name: string(v1.ResourceCPU), Weight: 1},
		{Name: string(v1.ResourceMemory), Weight: 1},
	}

	defaultForeignPodsDetect = ForeignPodsDetectAll

	defaultResyncMethod = CacheResyncAutodetect

	// Defaults for NetworkOverhead
	// DefaultWeightsName contains the default costs to be used by networkAware plugins
	DefaultWeightsName = "UserDefined"
	// DefaultNetworkTopologyName contains the networkTopology CR name to be used by networkAware plugins
	DefaultNetworkTopologyName = "nt-default"
)

// SetDefaults_CoschedulingArgs sets the default parameters for Coscheduling plugin.
func SetDefaults_CoschedulingArgs(obj *CoschedulingArgs) {
	if obj.PermitWaitingTimeSeconds == nil {
		obj.PermitWaitingTimeSeconds = &defaultPermitWaitingTimeSeconds
	}
	if obj.PodGroupBackoffSeconds == nil {
		obj.PodGroupBackoffSeconds = &defaultPodGroupBackoffSeconds
	}
}

// SetDefaults_NodeResourcesAllocatableArgs sets the defaults parameters for NodeResourceAllocatable.
func SetDefaults_NodeResourcesAllocatableArgs(obj *NodeResourcesAllocatableArgs) {
	if len(obj.Resources) == 0 {
		obj.Resources = defaultNodeResourcesAllocatableResourcesToWeightMap
	}

	if obj.Mode == "" {
		obj.Mode = defaultNodeResourcesAllocatableMode
	}
}

// SetDefaultTrimaranSpec sets the default parameters for common Trimaran plugins
func SetDefaultTrimaranSpec(args *TrimaranSpec) {
	if args.WatcherAddress == nil && args.MetricProvider.Type == "" {
		args.MetricProvider.Type = DefaultMetricProviderType
	}
	if args.MetricProvider.Type == Prometheus && args.MetricProvider.InsecureSkipVerify == nil {
		args.MetricProvider.InsecureSkipVerify = &DefaultInsecureSkipVerify
	}
}

// SetDefaults_TargetLoadPackingArgs sets the default parameters for TargetLoadPacking plugin
func SetDefaults_TargetLoadPackingArgs(args *TargetLoadPackingArgs) {
	SetDefaultTrimaranSpec(&args.TrimaranSpec)
	if args.DefaultRequests == nil {
		args.DefaultRequests = v1.ResourceList{v1.ResourceCPU: resource.MustParse(
			strconv.FormatInt(DefaultRequestsMilliCores, 10) + "m")}
	}
	if args.DefaultRequestsMultiplier == nil {
		args.DefaultRequestsMultiplier = &DefaultRequestsMultiplier
	}
	if args.TargetUtilization == nil || *args.TargetUtilization <= 0 {
		args.TargetUtilization = &DefaultTargetUtilizationPercent
	}
}

// SetDefaults_LoadVariationRiskBalancingArgs sets the default parameters for LoadVariationRiskBalancing plugin
func SetDefaults_LoadVariationRiskBalancingArgs(args *LoadVariationRiskBalancingArgs) {
	SetDefaultTrimaranSpec(&args.TrimaranSpec)
	if args.SafeVarianceMargin == nil || *args.SafeVarianceMargin < 0 {
		args.SafeVarianceMargin = &DefaultSafeVarianceMargin
	}
	if args.SafeVarianceSensitivity == nil || *args.SafeVarianceSensitivity < 0 {
		args.SafeVarianceSensitivity = &DefaultSafeVarianceSensitivity
	}
}

// SetDefaults_LowRiskOverCommitmentArgs sets the default parameters for LowRiskOverCommitment plugin
func SetDefaults_LowRiskOverCommitmentArgs(args *LowRiskOverCommitmentArgs) {
	SetDefaultTrimaranSpec(&args.TrimaranSpec)
	if args.SmoothingWindowSize == nil || *args.SmoothingWindowSize <= 0 {
		args.SmoothingWindowSize = &DefaultSmoothingWindowSize
	}
	if args.RiskLimitWeights == nil || len(args.RiskLimitWeights) == 0 {
		args.RiskLimitWeights = DefaultRiskLimitWeights
	} else {
		for r, w := range args.RiskLimitWeights {
			if w < 0 || w > 1 {
				args.RiskLimitWeights[r] = DefaultRiskLimitWeight
			}
		}
	}
}

// SetDefaults_NodeResourceTopologyMatchArgs sets the default parameters for NodeResourceTopologyMatch plugin.
func SetDefaults_NodeResourceTopologyMatchArgs(obj *NodeResourceTopologyMatchArgs) {
	if obj.ScoringStrategy == nil {
		obj.ScoringStrategy = &ScoringStrategy{
			Type:      LeastAllocated,
			Resources: defaultResourceSpec,
		}
	}

	if len(obj.ScoringStrategy.Resources) == 0 {
		// If no resources specified, use the default set.
		obj.ScoringStrategy.Resources = append(obj.ScoringStrategy.Resources, defaultResourceSpec...)
	}

	for i := range obj.ScoringStrategy.Resources {
		if obj.ScoringStrategy.Resources[i].Weight == 0 {
			obj.ScoringStrategy.Resources[i].Weight = 1
		}
	}

	if obj.Cache == nil {
		obj.Cache = &NodeResourceTopologyCache{}
	}
	if obj.Cache.ForeignPodsDetect == nil {
		obj.Cache.ForeignPodsDetect = &defaultForeignPodsDetect

	}
	if obj.Cache.ResyncMethod == nil {
		obj.Cache.ResyncMethod = &defaultResyncMethod
	}
}

// SetDefaults_PreemptionTolerationArgs reuses SetDefaults_DefaultPreemptionArgs
func SetDefaults_PreemptionTolerationArgs(obj *PreemptionTolerationArgs) {
	k8sschedulerconfigv1beta3.SetDefaults_DefaultPreemptionArgs((*schedulerconfigv1beta3.DefaultPreemptionArgs)(obj))
}

// SetDefaults_TopologicalSortArgs sets the default parameters for TopologicalSortArgs plugin.
func SetDefaults_TopologicalSortArgs(obj *TopologicalSortArgs) {
	if len(obj.Namespaces) == 0 {
		obj.Namespaces = []string{metav1.NamespaceDefault}
	}
}

// SetDefaults_NetworkOverheadArgs sets the default parameters for NetworkMinCostArgs plugin.
func SetDefaults_NetworkOverheadArgs(obj *NetworkOverheadArgs) {
	if len(obj.Namespaces) == 0 {
		obj.Namespaces = []string{metav1.NamespaceDefault}
	}

	if obj.WeightsName == nil {
		obj.WeightsName = &DefaultWeightsName
	}

	if obj.NetworkTopologyName == nil {
		obj.NetworkTopologyName = &DefaultNetworkTopologyName
	}
}
