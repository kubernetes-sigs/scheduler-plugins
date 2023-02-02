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
	"k8s.io/apimachinery/pkg/api/resource"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

func makePodByResourceList(resources *corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: *resources,
						Limits:   *resources,
					},
				},
			},
		},
	}
}

func makePodByResourceLists(resources ...corev1.ResourceList) *corev1.Pod {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{},
		},
	}

	for _, r := range resources {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Resources: corev1.ResourceRequirements{
				Requests: r,
				Limits:   r,
			},
		})
	}

	return pod
}

func makePodWithReqByResourceList(resources *corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: *resources,
					},
				},
			},
		},
	}
}

func makePodWithReqAndLimitByResourceList(resourcesReq, resourcesLim *corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: *resourcesReq,
						Limits:   *resourcesLim,
					},
				},
			},
		},
	}
}

func makeResourceListFromZones(zones topologyv1alpha2.ZoneList) corev1.ResourceList {
	result := make(corev1.ResourceList)
	for _, zone := range zones {
		for _, resInfo := range zone.Resources {
			resQuantity := resInfo.Available
			if quantity, ok := result[corev1.ResourceName(resInfo.Name)]; ok {
				resQuantity.Add(quantity)
			}
			result[corev1.ResourceName(resInfo.Name)] = resQuantity
		}
	}
	return result
}

func MakeTopologyResInfo(name, capacity, available string) topologyv1alpha2.ResourceInfo {
	return topologyv1alpha2.ResourceInfo{
		Name:      name,
		Capacity:  resource.MustParse(capacity),
		Available: resource.MustParse(available),
	}
}

func makePodByResourceListWithManyContainers(resources *corev1.ResourceList, containerCount int) *corev1.Pod {
	var containers []corev1.Container

	for i := 0; i < containerCount; i++ {
		containers = append(containers, corev1.Container{
			Resources: corev1.ResourceRequirements{
				Requests: *resources,
				Limits:   *resources,
			},
		})
	}
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: containers,
		},
	}
}
