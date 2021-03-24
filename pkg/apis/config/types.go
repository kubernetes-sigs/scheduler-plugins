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
	schedulerconfig "k8s.io/kube-scheduler/config/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CoschedulingArgs defines the parameters for Coscheduling plugin.
type CoschedulingArgs struct {
	metav1.TypeMeta

	// PermitWaitingTime is the wait timeout in seconds.
	PermitWaitingTimeSeconds int64
	// DeniedPGExpirationTimeSeconds is the expiration time of the denied podgroup store.
	DeniedPGExpirationTimeSeconds int64
	// KubeMaster is the url of api-server
	KubeMaster string
	// KubeConfigPath for scheduler
	KubeConfigPath string
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
	Resources []schedulerconfig.ResourceSpec `json:"resources,omitempty"`

	// Whether to prioritize nodes with least or most allocatable resources.
	Mode ModeType `json:"mode,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CapacitySchedulingArgs defines the scheduling parameters for CapacityScheduling plugin.
type CapacitySchedulingArgs struct {
	metav1.TypeMeta

	// KubeConfigPath is the path of kubeconfig.
	KubeConfigPath string
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
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// TargetLoadPackingArgs holds arguments used to configure TargetLoadPacking plugin.
type TargetLoadPackingArgs struct {
	metav1.TypeMeta

	// Default requests to use for best effort QoS
	DefaultRequests v1.ResourceList
	// Default requests multiplier for busrtable QoS
	DefaultRequestsMultiplier string
	// Node target CPU Utilization for bin packing
	TargetUtilization int64
	// Metric Provider to use when using load watcher as a library
	MetricProvider MetricProviderSpec
	// Address of load watcher service
	WatcherAddress string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LoadVariationRiskBalancingArgs holds arguments used to configure LoadVariationRiskBalancing plugin.
type LoadVariationRiskBalancingArgs struct {
	metav1.TypeMeta

	// Metric Provider to use when using load watcher as a library
	MetricProvider MetricProviderSpec
	// Address of load watcher service
	WatcherAddress string
	// Multiplier of standard deviation in risk value
	SafeVarianceMargin float64
	// Root power of standard deviation in risk value
	SafeVarianceSensitivity float64
}
