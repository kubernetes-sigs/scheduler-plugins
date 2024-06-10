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

package config

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CoschedulingArgs defines the parameters for Coscheduling plugin.
type CoschedulingArgs struct {
	metav1.TypeMeta

	// PermitWaitingTimeSeconds is the waiting timeout in seconds.
	PermitWaitingTimeSeconds int64
	// PodGroupBackoffSeconds is the backoff time in seconds before a pod group can be scheduled again.
	PodGroupBackoffSeconds int64
}

// ModeType is a "string" type.
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
	Resources []schedconfig.ResourceSpec `json:"resources,omitempty"`

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
	Type MetricProviderType
	// The address of the metric provider
	Address string
	// The authentication token of the metric provider
	Token string
	// Whether to enable the InsureSkipVerify options for https requests on Metric Providers.
	InsecureSkipVerify bool
}

// TrimaranSpec holds common parameters for trimaran plugins
type TrimaranSpec struct {
	// Metric Provider to use when using load watcher as a library
	MetricProvider MetricProviderSpec
	// Address of load watcher service
	WatcherAddress string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// TargetLoadPackingArgs holds arguments used to configure TargetLoadPacking plugin.
type TargetLoadPackingArgs struct {
	metav1.TypeMeta

	// Common parameters for trimaran plugins
	TrimaranSpec
	// Default requests to use for best effort QoS
	DefaultRequests v1.ResourceList
	// Default requests multiplier for busrtable QoS
	DefaultRequestsMultiplier string
	// Node target CPU Utilization for bin packing
	TargetUtilization int64
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LoadVariationRiskBalancingArgs holds arguments used to configure LoadVariationRiskBalancing plugin.
type LoadVariationRiskBalancingArgs struct {
	metav1.TypeMeta

	// Common parameters for trimaran plugins
	TrimaranSpec
	// Multiplier of standard deviation in risk value
	SafeVarianceMargin float64
	// Root power of standard deviation in risk value
	SafeVarianceSensitivity float64
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LowRiskOverCommitmentArgs holds arguments used to configure LowRiskOverCommitment plugin.
type LowRiskOverCommitmentArgs struct {
	metav1.TypeMeta

	// Common parameters for trimaran plugins
	TrimaranSpec
	// The number of windows over which usage data metrics are smoothed
	SmoothingWindowSize int64
	// Resources fractional weight of risk due to limits specification [0,1]
	RiskLimitWeights map[v1.ResourceName]float64
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

// ScoringStrategy define ScoringStrategyType for node resource topology plugin
type ScoringStrategy struct {
	// Type selects which strategy to run.
	Type ScoringStrategyType

	// Resources a list of pairs <resource, weight> to be considered while scoring
	// allowed weights start from 1.
	Resources []schedconfig.ResourceSpec
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

// CacheInformerMode is a "string" type
type CacheInformerMode string

const (
	CacheInformerShared    CacheInformerMode = "Shared"
	CacheInformerDedicated CacheInformerMode = "Dedicated"
)

// CacheResyncScope is a "string" type
type CacheResyncScope string

const (
	CacheResyncScopeAll           CacheResyncScope = "All"
	CacheResyncScopeOnlyResources CacheResyncScope = "OnlyResources"
)

// NodeResourceTopologyCache define configuration details for the NodeResourceTopology cache.
type NodeResourceTopologyCache struct {
	// ForeignPodsDetect sets how foreign pods should be handled.
	// Foreign pods are pods detected running on nodes managed by a NodeResourceTopologyMatch-enabled
	// scheduler, but not scheduled by this scheduler instance, likely because this is running as
	// secondary scheduler. To make sure the cache is consistent, foreign pods need to be handled.
	// Has no effect if caching is disabled (CacheResyncPeriod is zero) or
	// if DiscardReservedNodes is enabled. If unspecified, default is "All".
	ForeignPodsDetect *ForeignPodsDetectMode
	// ResyncMethod sets how the resync behaves to compute the expected node state.
	// "All" consider all pods to compute the node state. "OnlyExclusiveResources" consider
	// only pods regardless of their QoS which have exclusive resources assigned to their
	// containers (CPUs, devices...).
	// Has no effect if caching is disabled (CacheResyncPeriod is zero) or if DiscardReservedNodes
	// is enabled. "Autodetect" is the default, reads hint from NRT objects. Fallback is "All".
	ResyncMethod *CacheResyncMethod
	// InformerMode controls the channel the cache uses to get updates about pods.
	// "Shared" uses the default settings; "Dedicated" creates a specific subscription which is
	// guaranteed to best suit the cache needs, at cost of one extra connection.
	// If unspecified, default is "Dedicated"
	InformerMode *CacheInformerMode
	// ResyncScope controls which changes the resync logic monitors to trigger an update.
	// "All" consider both Attributes (metadata, node config details) and per-NUMA resources,
	// while "OnlyResources" consider only per-NUMA resource values. The default is
	// "All" to make the code react to node config changes avoiding reboots.
	// Use "OnlyResources" to restore the previous behavior.
	ResyncScope *CacheResyncScope
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeResourceTopologyMatchArgs holds arguments used to configure the NodeResourceTopologyMatch plugin
type NodeResourceTopologyMatchArgs struct {
	metav1.TypeMeta

	// ScoringStrategy a scoring model that determine how the plugin will score the nodes.
	ScoringStrategy ScoringStrategy
	// If > 0, enables the caching facilities of the reserve plugin - which must be enabled
	CacheResyncPeriodSeconds int64
	// if set to true, exclude node from scheduling if there are any reserved pods for given node
	// this option takes precedence over CacheResyncPeriodSeconds
	// if DiscardReservedNodes is enabled, CacheResyncPeriodSeconds option is noop
	DiscardReservedNodes bool
	// Cache enables to fine tune the caching behavior
	Cache *NodeResourceTopologyCache
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PreemptionTolerationArgs reuses DefaultPluginArgs.
type PreemptionTolerationArgs schedconfig.DefaultPreemptionArgs

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type TopologicalSortArgs struct {
	metav1.TypeMeta

	// Namespaces to be considered by TopologySort plugin
	Namespaces []string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type NetworkOverheadArgs struct {
	metav1.TypeMeta

	// Namespaces to be considered by NetworkMinCost plugin
	Namespaces []string

	// Preferred weights (Default: UserDefined)
	WeightsName string

	// The NetworkTopology CRD name
	NetworkTopologyName string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type SySchedArgs struct {
	metav1.TypeMeta

	// CR namespace of the default profile for all system calls
	DefaultProfileNamespace string

	// CR name of the default profile for all system calls
	DefaultProfileName string
}
