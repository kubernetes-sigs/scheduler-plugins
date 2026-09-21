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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
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
	nrtResources := sets.New(corev1.ResourceName("veryfast.io/fpga"))
	tcases := coreTestCases()
	for _, tt := range tcases {
		t.Run(tt.name, func(t *testing.T) {
			got := AreExclusiveForPod(tt.pod, nrtResources)
			if got != tt.expectedExclusive {
				t.Errorf("%s: exclusive resources detected %v expected %v", tt.name, got, tt.expectedExclusive)
			}
		})
	}
}

func TestAreExclusiveForPodNRTScoped(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		nrtResources sets.Set[corev1.ResourceName]
		expected     bool
	}{
		{
			name: "device-in-nrt-is-exclusive",
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
								Requests: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			nrtResources: sets.New(corev1.ResourceName("veryfast.io/fpga")),
			expected:     true,
		},
		{
			name: "device-not-in-nrt-is-not-exclusive",
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
								Requests: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			nrtResources: sets.New[corev1.ResourceName](),
			expected:     false,
		},
		{
			name: "nil-nrt-resources-device-not-exclusive",
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
								Requests: corev1.ResourceList{
									corev1.ResourceName("veryfast.io/fpga"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "gu-cpu-exclusive-regardless-of-nrt",
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
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AreExclusiveForPod(tt.pod, tt.nrtResources)
			if got != tt.expected {
				t.Errorf("%s: exclusive resources detected %v expected %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestGetExclusive(t *testing.T) {
	fpga := corev1.ResourceName("veryfast.io/fpga")
	nrtWithFPGA := sets.New(fpga, corev1.ResourceCPU, corev1.ResourceMemory)

	tests := []struct {
		name         string
		qos          corev1.PodQOSClass
		container    corev1.Container
		nrtResources sets.Set[corev1.ResourceName]
		expected     corev1.ResourceList
	}{
		{
			name:         "empty requests",
			qos:          corev1.PodQOSGuaranteed,
			container:    corev1.Container{Name: "cnt"},
			nrtResources: nrtWithFPGA,
			expected:     corev1.ResourceList{},
		},
		{
			name: "guaranteed integral cpu and memory",
			qos:  corev1.PodQOSGuaranteed,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
			nrtResources: nrtWithFPGA,
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
		{
			name: "guaranteed fractional cpu excluded",
			qos:  corev1.PodQOSGuaranteed,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			},
			nrtResources: nrtWithFPGA,
			expected: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		{
			name: "burstable native resources excluded",
			qos:  corev1.PodQOSBurstable,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
			nrtResources: nrtWithFPGA,
			expected:     corev1.ResourceList{},
		},
		{
			name: "device in nrt is exclusive for any qos",
			qos:  corev1.PodQOSBurstable,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						fpga: resource.MustParse("1"),
					},
				},
			},
			nrtResources: nrtWithFPGA,
			expected: corev1.ResourceList{
				fpga: resource.MustParse("1"),
			},
		},
		{
			name: "device not in nrt is excluded",
			qos:  corev1.PodQOSGuaranteed,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						fpga: resource.MustParse("1"),
					},
				},
			},
			nrtResources: sets.New(corev1.ResourceCPU, corev1.ResourceMemory),
			expected:     corev1.ResourceList{},
		},
		{
			name: "mixed requests return only exclusive subset",
			qos:  corev1.PodQOSGuaranteed,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
						fpga:                  resource.MustParse("2"),
					},
				},
			},
			nrtResources: nrtWithFPGA,
			expected: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				fpga:                  resource.MustParse("2"),
			},
		},
		{
			name: "hugepages are exclusive for guaranteed pods",
			qos:  corev1.PodQOSGuaranteed,
			container: corev1.Container{
				Name: "cnt",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName("hugepages-2Mi"): resource.MustParse("64Mi"),
					},
				},
			},
			nrtResources: sets.New(corev1.ResourceName("hugepages-2Mi")),
			expected: corev1.ResourceList{
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("64Mi"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetExclusive(tt.qos, tt.container, tt.nrtResources)
			if len(got) != len(tt.expected) {
				t.Fatalf("mismatching number of resources; got\n%v\nexpected\n%v", got, tt.expected)
			}
			for name, qty := range got {
				other, ok := tt.expected[name]
				if !ok || qty.Cmp(other) != 0 {
					t.Fatalf("mismatching resource quantity; got\n%v\nexpected\n%v", got, tt.expected)
				}
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
			expectedExclusive: false,
		},
		{
			name: "single-sidecar-initcontainer-gu-no-devs",
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
							RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
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
			expectedExclusive: false,
		},
		{
			name: "single-sidecar-initcontainer-devs-only",
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
							RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
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
