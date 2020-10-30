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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedulerconfig "k8s.io/kube-scheduler/config/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type CoschedulingArgs struct {
	metav1.TypeMeta

	// PermitWaitingTime is the wait timeout in seconds.
	PermitWaitingTimeSeconds int64
	// PodGroupGCInterval is the period to run gc of PodGroup in seconds.
	PodGroupGCIntervalSeconds int64
	// If the deleted PodGroup stays longer than the PodGroupExpirationTime,
	// the PodGroup will be deleted from PodGroupInfos.
	PodGroupExpirationTimeSeconds int64
	// KubeMaster is the url of api-server
	KubeMaster string
	// KubeConfigPath for scheduler
	KubeConfigPath string
}

// modes type.
type ModeType string

const (
	Least ModeType = "Least"
	Most  ModeType = "Most"
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

type CapacitySchedulingArgs struct {
	metav1.TypeMeta

	// KubeConfigPath is the path of kubeconfig.
	KubeConfigPath string
}
