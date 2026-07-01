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
	"k8s.io/apimachinery/pkg/util/sets"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
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

// AreExclusiveForPod checks if the given pod's containers are consuming exclusive resources
// in the steady state of the pod, i.e. after the irrestartable init containers have finished running.
func AreExclusiveForPod(pod *corev1.Pod, nrtResources sets.Set[corev1.ResourceName]) bool {
	qos := v1qos.GetPodQOS(pod)

	// filter out init containers with restart policy other than Always because these are *supposed* to
	// run fast and finish, hence not consuming exclusive resources in a steady state while the pod is Running.
	restartableInitCnts := []corev1.Container{}
	for _, ctr := range pod.Spec.InitContainers {
		if !util.IsSidecarInitContainer(&ctr) {
			continue
		}
		restartableInitCnts = append(restartableInitCnts, ctr)
	}
	return areExclusiveForAnyContainer(qos, append(restartableInitCnts, pod.Spec.Containers...), nrtResources)
}

func areExclusiveForAnyContainer(qos corev1.PodQOSClass, containers []corev1.Container, nrtResources sets.Set[corev1.ResourceName]) bool {
	for _, ctr := range containers {
		for resource, quantity := range ctr.Resources.Requests {
			if ok := IsExclusive(qos, resource, quantity, nrtResources); ok {
				return true
			}
		}
	}
	return false
}

func IsExclusive(qos corev1.PodQOSClass, resource corev1.ResourceName, quantity resource.Quantity, nrtResources sets.Set[corev1.ResourceName]) bool {
	// devices accessed via device plugins are non-shareable.
	// extended resources are not necessarily bound to the node topology which means they may not be exclusive.
	// the fact that a resource is being tracked in the NRT object provides certantiy about its topological criteria.
	if !v1helper.IsNativeResource(resource) {
		return nrtResources.Has(resource)
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
