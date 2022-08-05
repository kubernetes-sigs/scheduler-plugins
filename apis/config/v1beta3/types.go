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

// ScoringStrategyType is a "string" type.
type ScoringStrategyType string

const (
	// MostAllocated strategy favors node with the least amount of available resource
	MostAllocated ScoringStrategyType = "MostAllocated"
	// BalancedAllocation strategy favors nodes with balanced resource usage rate
	BalancedAllocation ScoringStrategyType = "BalancedAllocation"
	// LeastAllocated strategy favors node with the most amount of available resource
	LeastAllocated ScoringStrategyType = "LeastAllocated"
)

type ScoringStrategy struct {
	Type      ScoringStrategyType                   `json:"type,omitempty"`
	Resources []schedulerconfigv1beta3.ResourceSpec `json:"resources,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeResourceTopologyMatchArgs holds arguments used to configure the NodeResourceTopologyMatch plugin
type NodeResourceTopologyMatchArgs struct {
	metav1.TypeMeta `json:",inline"`

	ScoringStrategy *ScoringStrategy `json:"scoringStrategy,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PreemptionTolerationArgs reuses DefaultPluginArgs.
type PreemptionTolerationArgs schedulerconfigv1beta3.DefaultPreemptionArgs
