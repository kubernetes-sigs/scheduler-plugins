/*
Copyright 2023 The Kubernetes Authors.

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

package resourcerequests

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
)

func IncludeNonNative(pod *corev1.Pod) bool {
	for _, initContainer := range pod.Spec.InitContainers {
		for resource := range initContainer.Resources.Requests {
			if !v1helper.IsNativeResource(resource) {
				return true
			}
		}
	}
	for _, container := range pod.Spec.Containers {
		for resource := range container.Resources.Requests {
			if !v1helper.IsNativeResource(resource) {
				return true
			}
		}
	}
	return false
}
func AreExclusiveForPod(pod *corev1.Pod) bool {
	qos := v1qos.GetPodQOS(pod)
	return areExclusiveForAnyContainer(qos, append(pod.Spec.InitContainers, pod.Spec.Containers...))
}

func areExclusiveForAnyContainer(qos corev1.PodQOSClass, containers []corev1.Container) bool {
	for _, ctr := range containers {
		for resource, quantity := range ctr.Resources.Requests {
			if ok := IsExclusive(qos, resource, quantity); ok {
				return true
			}
		}
	}
	return false
}

func IsExclusive(qos corev1.PodQOSClass, resource corev1.ResourceName, quantity resource.Quantity) bool {
	// devices accessed via device plugins are non-shareable
	// note until we reach better clarity we treat extended resources as devices
	if !v1helper.IsNativeResource(resource) {
		return true
	}
	if qos != corev1.PodQOSGuaranteed {
		// nothing more we can check, bail out ASAP
		return false
	}
	if resource == corev1.ResourceCPU {
		// integral CPU quantity == exclusive CPU
		if (quantity.Value()*1000 == quantity.MilliValue()) && quantity.Value() > 0 {
			return true
		}
	}
	if resource == corev1.ResourceMemory || v1helper.IsHugePageResourceName(resource) {
		if quantity.Value() > 0 {
			return true
		}
	}
	return false
}
