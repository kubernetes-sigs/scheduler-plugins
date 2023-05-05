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

package cache

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsForeignPod(t *testing.T) {
	tests := []struct {
		name         string
		profileNames []string
		pod          *corev1.Pod
		expected     bool
	}{
		{
			name: "empty",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
			},
		},
		{
			name:         "no-node-no-profile",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
			},
		},
		{
			name:         "node-no-profile-no-devs",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName: "random-node",
					Containers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("4"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("4"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:         "node-no-profile-no-devs",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName: "random-node",
					InitContainers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("4"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("4"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:         "node-no-profile-devs-only",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName: "random-node",
					Containers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:         "node-no-profile-devs-only",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName: "random-node",
					InitContainers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:         "node-profile",
			profileNames: []string{"secondary-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName:      "random-node",
					SchedulerName: "secondary-scheduler",
				},
			},
		},
		{
			name:         "node-multi-profile",
			profileNames: []string{"secondary-scheduler-A", "secondary-scheduler-B", "fancy-scheduler"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName:      "random-node",
					SchedulerName: "secondary-scheduler-B",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, profileName := range tt.profileNames {
				RegisterSchedulerProfileName(profileName)
			}
			defer CleanRegisteredSchedulerProfileNames()

			got := IsForeignPod(tt.pod)
			if got != tt.expected {
				t.Errorf("%s: pod %q foreign status got %v expected %v", tt.name, tt.pod.Name, got, tt.expected)
			}
		})
	}
}
