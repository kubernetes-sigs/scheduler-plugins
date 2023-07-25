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

package v1beta3

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedulerconfigv1beta3 "k8s.io/kube-scheduler/config/v1beta3"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CoschedulingArgs defines the scheduling parameters for Coscheduling plugin.
type CoschedulingArgs struct {
	metav1.TypeMeta `json:",inline"`

	// PermitWaitingTimeSeconds is the waiting timeout in seconds.
	PermitWaitingTimeSeconds *int64 `json:"permitWaitingTimeSeconds,omitempty"`
	// PodGroupBackoffSeconds is the backoff time in seconds before a pod group can be scheduled again.
	PodGroupBackoffSeconds *int64 `json:"podGroupBackoffSeconds,omitempty"`
}

// ModeType is a type "string".
type ModeType string

const (
	// Least is the string "Least".
	Least ModeType = "Least"
	// Most is the string "Most".
	Most ModeType = "Most"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeResourcesAllocatableArgs holds arguments used to configure NodeResourcesAllocatable plugin.
type NodeResourcesAllocatableArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Resources to be considered when scoring.
	// Allowed weights start from 1.
	// An example resource set might include "cpu" (millicores) and "memory" (bytes)
	// with weights of 1<<20 and 1 respectfully. That would mean 1 MiB has equivalent
	// weight as 1 millicore.
	Resources []schedulerconfigv1beta3.ResourceSpec `json:"resources,omitempty"`

	// Whether to prioritize nodes with least or most allocatable resources.
	Mode ModeType `json:"mode,omitempty"`
}

// MetricProviderType is a "string" type.
type MetricProviderType string

const (
	KubernetesMetricsServer MetricProviderType = "KubernetesMetricsServer"
	Prometheus              MetricProviderType = "Prometheus"
	SignalFx                MetricProviderType = "SignalFx"
)

// Denote the spec of the metric provider
type MetricProviderSpec struct {
	// Types of the metric provider
	Type MetricProviderType `json:"type,omitempty"`
	// The address of the metric provider
	Address *string `json:"address,omitempty"`
	// The authentication token of the metric provider
	Token *string `json:"token,omitempty"`
	// Whether to enable the InsureSkipVerify options for https requests on Prometheus Metric Provider.
	InsecureSkipVerify *bool `json:"insecureSkipVerify,omitempty"`
}

// TrimaranSpec holds common parameters for trimaran plugins
type TrimaranSpec struct {
	// Metric Provider specification when using load watcher as library
	MetricProvider MetricProviderSpec `json:"metricProvider,omitempty"`
	// Address of load watcher service
	WatcherAddress *string `json:"watcherAddress,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// TargetLoadPackingArgs holds arguments used to configure TargetLoadPacking plugin.
type TargetLoadPackingArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Common parameters for trimaran plugins
	TrimaranSpec `json:",inline"`
	// Default requests to use for best effort QoS
	DefaultRequests v1.ResourceList `json:"defaultRequests,omitempty"`
	// Default requests multiplier for busrtable QoS
	DefaultRequestsMultiplier *string `json:"defaultRequestsMultiplier,omitempty"`
	// Node target CPU Utilization for bin packing
	TargetUtilization *int64 `json:"targetUtilization,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// LoadVariationRiskBalancingArgs holds arguments used to configure LoadVariationRiskBalancing plugin.
type LoadVariationRiskBalancingArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Common parameters for trimaran plugins
	TrimaranSpec `json:",inline"`
	// Multiplier of standard deviation in risk value
	SafeVarianceMargin *float64 `json:"safeVarianceMargin,omitempty"`
	// Root power of standard deviation in risk value
	SafeVarianceSensitivity *float64 `json:"safeVarianceSensitivity,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// LowRiskOverCommitmentArgs holds arguments used to configure LowRiskOverCommitment plugin.
type LowRiskOverCommitmentArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Common parameters for trimaran plugins
	TrimaranSpec `json:",inline"`
	// The number of windows over which usage data metrics are smoothed
	SmoothingWindowSize *int64 `json:"smoothingWindowSize,omitempty"`
	// Resources fractional weight of risk due to limits specification [0,1]
	RiskLimitWeights map[v1.ResourceName]float64 `json:"riskLimitWeights,omitempty"`
}

// ScoringStrategyType is a "string" type.
type ScoringStrategyType string

const (
	// MostAllocated strategy favors node with the least amount of available resource
	MostAllocated ScoringStrategyType = "MostAllocated"
	// BalancedAllocation strategy favors nodes with balanced resource usage rate
	BalancedAllocation ScoringStrategyType = "BalancedAllocation"
	// LeastAllocated strategy favors node with the most amount of available resource
	LeastAllocated ScoringStrategyType = "LeastAllocated"
	// LeastNUMANodes strategy favors nodes which requires least amount of NUMA nodes to satisfy resource requests for given pod
	LeastNUMANodes ScoringStrategyType = "LeastNUMANodes"
)

type ScoringStrategy struct {
	Type      ScoringStrategyType                   `json:"type,omitempty"`
	Resources []schedulerconfigv1beta3.ResourceSpec `json:"resources,omitempty"`
}

// ForeignPodsDetectMode is a "string" type.
type ForeignPodsDetectMode string

const (
	ForeignPodsDetectNone                   ForeignPodsDetectMode = "None"
	ForeignPodsDetectAll                    ForeignPodsDetectMode = "All"
	ForeignPodsDetectOnlyExclusiveResources ForeignPodsDetectMode = "OnlyExclusiveResources"
)

// CacheResyncMethod is a "string" type.
type CacheResyncMethod string

const (
	CacheResyncAutodetect             CacheResyncMethod = "Autodetect"
	CacheResyncAll                    CacheResyncMethod = "All"
	CacheResyncOnlyExclusiveResources CacheResyncMethod = "OnlyExclusiveResources"
)

// NodeResourceTopologyCache define configuration details for the NodeResourceTopology cache.
type NodeResourceTopologyCache struct {
	// ForeignPodsDetect sets how foreign pods should be handled.
	// Foreign pods are pods detected running on nodes managed by a NodeResourceTopologyMatch-enabled
	// scheduler, but not scheduled by this scheduler instance, likely because this is running as
	// secondary scheduler. To make sure the cache is consistent, foreign pods need to be handled.
	// Has no effect if caching is disabled (CacheResyncPeriod is zero) or if DiscardReservedNodes
	// is enabled. If unspecified, default is "All". Use "None" to disable.
	ForeignPodsDetect *ForeignPodsDetectMode `json:"foreignPodsDetect,omitempty"`
	// ResyncMethod sets how the resync behaves to compute the expected node state.
	// "All" consider all pods to compute the node state. "OnlyExclusiveResources" consider
	// only pods regardless of their QoS which have exclusive resources assigned to their
	// containers (CPUs, devices...).
	// Has no effect if caching is disabled (CacheResyncPeriod is zero) or if DiscardReservedNodes
	// is enabled. "Autodetect" is the default, reads hint from NRT objects. Fallback is "All".
	ResyncMethod *CacheResyncMethod `json:"resyncMethod,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeResourceTopologyMatchArgs holds arguments used to configure the NodeResourceTopologyMatch plugin
type NodeResourceTopologyMatchArgs struct {
	metav1.TypeMeta `json:",inline"`

	// ScoringStrategy a scoring model that determine how the plugin will score the nodes.
	ScoringStrategy *ScoringStrategy `json:"scoringStrategy,omitempty"`
	// CacheResyncPeriodSeconds sets the resync period, in seconds, between the internal
	// NodeResourceTopoology cache and the apiserver. If present and greater than zero,
	// implicitely enables the caching. If zero, disables the caching entirely.
	// If the cache is enabled, the Reserve plugin must be enabled.
	CacheResyncPeriodSeconds *int64 `json:"cacheResyncPeriodSeconds,omitempty"`
	// if set to true, exclude node from scheduling if there are any reserved pods for given node
	// this option takes precedence over CacheResyncPeriodSeconds
	// if DiscardReservedNodes is enabled, CacheResyncPeriodSeconds option is noop
	DiscardReservedNodes bool `json:"discardReservedNodes,omitempty"`
	// Cache enables to fine tune the caching behavior
	Cache *NodeResourceTopologyCache `json:"cache,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PreemptionTolerationArgs reuses DefaultPluginArgs.
type PreemptionTolerationArgs schedulerconfigv1beta3.DefaultPreemptionArgs

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type TopologicalSortArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Namespaces to be considered by TopologySort plugin
	Namespaces []string `json:"namespaces,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type NetworkOverheadArgs struct {
	metav1.TypeMeta `json:",inline"`

	// Namespaces to be considered by NetworkMinCost plugin
	Namespaces []string `json:"namespaces,omitempty"`

	// Preferred weights (Default: UserDefined)
	WeightsName *string `json:"weightsName,omitempty"`

	// The NetworkTopology CRD name
	NetworkTopologyName *string `json:"networkTopologyName,omitempty"`
}
