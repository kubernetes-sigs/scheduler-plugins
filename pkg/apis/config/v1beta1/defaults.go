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

package v1beta1

import (
	schedulerconfig "k8s.io/kube-scheduler/config/v1"
)

var (
	defaultPermitWaitingTimeSeconds      int64 = 10
	defaultPodGroupGCIntervalSeconds     int64 = 30
	defaultPodGroupExpirationTimeSeconds int64 = 600

	defaultNodeResourcesAllocatableMode ModeType = Least

	// defaultResourcesToWeightMap is used to set the default resourceToWeight map for CPU and memory
	// used by the NodeResourcesAllocatable scoring plugin.
	// The base unit for CPU is millicore, while the base using for memory is a byte.
	// The default CPU weight is 1<<20 and default memory weight is 1. That means a millicore
	// has a weighted score equivalent to 1 MiB.
	defaultNodeResourcesAllocatableResourcesToWeightMap = []schedulerconfig.ResourceSpec{
		{Name: "cpu", Weight: 1 << 20}, {Name: "memory", Weight: 1},
	}

	defaultKubeConfigPath string = "/etc/kubernetes/scheduler.conf"
)

func SetDefaults_CoschedulingArgs(obj *CoschedulingArgs) {
	if obj.PermitWaitingTimeSeconds == nil {
		obj.PermitWaitingTimeSeconds = &defaultPermitWaitingTimeSeconds
	}
	if obj.PodGroupGCIntervalSeconds == nil {
		obj.PodGroupGCIntervalSeconds = &defaultPodGroupGCIntervalSeconds
	}
	if obj.PodGroupExpirationTimeSeconds == nil {
		obj.PodGroupExpirationTimeSeconds = &defaultPodGroupExpirationTimeSeconds
	}
}

func SetDefaults_NodeResourcesAllocatableArgs(obj *NodeResourcesAllocatableArgs) {
	if obj.Resources == nil {
		obj.Resources = defaultNodeResourcesAllocatableResourcesToWeightMap
	}

	if obj.Mode == nil {
		obj.Mode = &defaultNodeResourcesAllocatableMode
	}
}

func SetDefaults_CapacitySchedulingArgs(obj *CapacitySchedulingArgs) {
	if obj.KubeConfigPath == nil {
		obj.KubeConfigPath = &defaultKubeConfigPath
	}
}
