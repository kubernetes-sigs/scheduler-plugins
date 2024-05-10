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

package noderesourcetopology

import (
	corev1 "k8s.io/api/core/v1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
)

func isHostLevelResource(resource corev1.ResourceName) bool {
	// host-level resources are resources which *may* not be bound to NUMA nodes.
	// A Key example is generic [ephemeral] storage which doesn't expose NUMA affinity.
	if resource == corev1.ResourceEphemeralStorage {
		return true
	}
	if resource == corev1.ResourceStorage {
		return true
	}
	if !v1helper.IsNativeResource(resource) {
		return true
	}
	return false
}
