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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type testCase struct {
	name              string
	pod               *corev1.Pod
	expectedNonNative bool
	expectedExclusive bool
}

func TestIncludeNonNative(t *testing.T) {
	tcases := coreTestCases()
	for _, tt := range tcases {
		t.Run(tt.name, func(t *testing.T) {
			got := IncludeNonNative(tt.pod)
			if got != tt.expectedNonNative {
				t.Errorf("%s: non-native resources detected %v expected %v", tt.name, got, tt.expectedNonNative)
			}
		})
	}
}

func TestAreExclusiveForPod(t *testing.T) {
	tcases := coreTestCases()
	for _, tt := range tcases {
		t.Run(tt.name, func(t *testing.T) {
			got := AreExclusiveForPod(tt.pod)
			if got != tt.expectedExclusive {
				t.Errorf("%s: exclusive resources detected %v expected %v", tt.name, got, tt.expectedExclusive)
			}
		})
	}
}

func coreTestCases() []testCase {
	return []testCase{
		{
			name: "no containers",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
			},
			expectedNonNative: false,
			expectedExclusive: false,
		},
		{
			name: "single-container-gu-no-devs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
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
			expectedNonNative: false,
			expectedExclusive: true,
		},
		{
			name: "single-initcontainer-gu-no-devs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
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
			expectedNonNative: false,
			expectedExclusive: true,
		},
		{
			name: "single-container-devs-only",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
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
			expectedNonNative: true,
			expectedExclusive: true,
		},
		{
			name: "single-initcontainer-devs-only",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
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
			expectedNonNative: true,
			expectedExclusive: true,
		},
		{
			name: "single-container-gu-core-and-devs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:                      resource.MustParse("8"),
									corev1.ResourceMemory:                   resource.MustParse("16Gi"),
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                      resource.MustParse("8"),
									corev1.ResourceMemory:                   resource.MustParse("16Gi"),
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			expectedNonNative: true,
			expectedExclusive: true,
		},
		{
			name: "single-container-nongu-cpus-and-devs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:                      resource.MustParse("8"),
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                      resource.MustParse("8"),
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			expectedNonNative: true,
			expectedExclusive: true,
		},
		{
			name: "single-container-nongu-cpus-only",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cnt",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("8"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("8"),
								},
							},
						},
					},
				},
			},
			expectedNonNative: false,
			expectedExclusive: false,
		},
	}
}
