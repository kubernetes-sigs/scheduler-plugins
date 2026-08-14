/*
Copyright The Kubernetes Authors.

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

package preemption

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/google/go-cmp/cmp"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/numaplacement"
)

func TestGetNRTPostPodsEviction(t *testing.T) {
	testcases := []struct {
		name               string
		nrt                *topologyv1alpha2.NodeResourceTopology
		victims            []corev1.Pod
		numaPlacementInfo  *numaplacement.EncodedInfo
		expectedUpdatedNRT *topologyv1alpha2.NodeResourceTopology
		expectedError      string
	}{
		{
			name:               "no victims", // does not happen in reality but still
			nrt:                getTestNRT(),
			victims:            []corev1.Pod{},
			expectedUpdatedNRT: getTestNRT(),
			numaPlacementInfo:  getTestEncodedInfo10Containers(),
			expectedError:      "no victims found, cannot process eviction simulation",
		},
		{
			name: "empty numa placement info with victims",
			nrt:  getTestNRT(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod-0",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSGuaranteed,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "container-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("1"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
								},
							},
						},
					},
				},
			},
			expectedUpdatedNRT: getTestNRT(),
			expectedError:      "numa placement info not found, cannot process eviction simulation",
		},
		{
			name: "victims with non-exclusive resources",
			nrt:  getTestNRT(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod-0",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSBestEffort,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "container-0",
							},
						},
					},
				},
			},
			numaPlacementInfo:  getTestEncodedInfo10Containers(),
			expectedUpdatedNRT: getTestNRT(),
			expectedError:      "no resources to add, cannot process eviction simulation",
		},
		{
			name: "mixed victims with exclusive resources",
			nrt:  getTestNRT(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns-a",
						Name:      "pod-0",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSBurstable,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "cnt-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("1"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
									Limits: corev1.ResourceList{
										"cpu":                        resource.MustParse("2"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
								},
							},
							{
								Name: "cnt-1",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("1"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("2"),
									},
									Limits: corev1.ResourceList{
										"cpu":                        resource.MustParse("2"),
										"memory":                     resource.MustParse("200Mi"),
										"example-device.com/deviceA": resource.MustParse("2"),
									},
								},
							},
							{
								Name: "cnt-2",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":    resource.MustParse("1"),
										"memory": resource.MustParse("100Mi"),
									},
									Limits: corev1.ResourceList{
										"cpu":    resource.MustParse("2"),
										"memory": resource.MustParse("200Mi"),
									},
								},
							},
							{
								Name: "cnt-3",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("1"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
									Limits: corev1.ResourceList{
										"cpu":                        resource.MustParse("2"),
										"memory":                     resource.MustParse("200Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns-b",
						Name:      "pod-1",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSBestEffort,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "cnt-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"example-device.com/deviceB": resource.MustParse("3"),
									},
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns-b",
						Name:      "pod-2",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSGuaranteed,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "cnt-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"example-device.com/deviceB": resource.MustParse("3"),
										"cpu":                        resource.MustParse("2"),
										"memory":                     resource.MustParse("100Mi"),
									},
								},
							},
						},
					},
				},
			},
			numaPlacementInfo: getTestEncodedInfo10Containers(),
			expectedUpdatedNRT: &topologyv1alpha2.NodeResourceTopology{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-0",
				},
				Zones: []topologyv1alpha2.Zone{
					{
						Name: "node-0",
						Resources: []topologyv1alpha2.ResourceInfo{
							{
								Name:        "cpu",
								Capacity:    resource.MustParse("10"),
								Allocatable: resource.MustParse("10"),
								Available:   resource.MustParse("1"),
							},
							{
								Name:        "memory",
								Capacity:    resource.MustParse("500Mi"),
								Allocatable: resource.MustParse("500Mi"),
								Available:   resource.MustParse("100Mi"),
							},
							{
								Name:        "example-device.com/deviceA",
								Capacity:    resource.MustParse("8"),
								Allocatable: resource.MustParse("8"),
								Available:   resource.MustParse("5"),
							},
						},
					},
					{
						Name: "node-1",
						Resources: []topologyv1alpha2.ResourceInfo{
							{
								Name:        "cpu",
								Capacity:    resource.MustParse("10"),
								Allocatable: resource.MustParse("10"),
								Available:   resource.MustParse("3"), // only for the guaranteed pod containers
							},
							{
								Name:        "memory",
								Capacity:    resource.MustParse("500Mi"),
								Allocatable: resource.MustParse("500Mi"),
								Available:   resource.MustParse("200Mi"), // only for the guaranteed pod containers
							},
							{
								Name:        "example-device.com/deviceB",
								Capacity:    resource.MustParse("8"),
								Allocatable: resource.MustParse("8"),
								Available:   resource.MustParse("8"),
							},
						},
					},
				},
			},
		},
		{
			name: "victim not found in numa placement info",
			nrt:  getTestNRT(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "newpod",
						Namespace: "ns-a",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSGuaranteed,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "cnt-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("1"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("1"),
									},
								},
							},
						},
					},
				},
			},
			numaPlacementInfo:  getTestEncodedInfo10Containers(),
			expectedUpdatedNRT: getTestNRT(),
			expectedError:      "no resources to add, cannot process eviction simulation",
		},
		{
			name: "resources release exceeds allocatable",
			nrt:  getTestNRT(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns-a",
						Name:      "pod-0",
					},
					Status: corev1.PodStatus{
						QOSClass: corev1.PodQOSGuaranteed,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "cnt-0",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										"cpu":                        resource.MustParse("15"),
										"memory":                     resource.MustParse("100Mi"),
										"example-device.com/deviceA": resource.MustParse("20"),
									},
								},
							},
						},
					},
				},
			},
			numaPlacementInfo:  getTestEncodedInfo10Containers(),
			expectedUpdatedNRT: getTestNRT(),
			expectedError:      "resource release request exceeds NUMA allocatable",
		},
		{
			name:              "numa placement info with no containers",
			nrt:               getTestNRT(),
			numaPlacementInfo: numaplacement.NewEncodedInfo(),
			victims: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod-0",
					},
				},
			},
			expectedUpdatedNRT: getTestNRT(),
			expectedError:      "no containers found in numa placement info, cannot process eviction simulation",
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			updatedNRT, err := GetNRTPostPodsEviction(klog.Background(), testcase.nrt, testcase.victims, testcase.numaPlacementInfo)
			if err != nil && testcase.expectedError == "" {
				t.Fatalf("expected no error, got %v", err)
			}

			if err == nil && testcase.expectedError != "" {
				t.Fatalf("expected error %s, got nil", testcase.expectedError)
			}

			if err != nil && err.Error() != testcase.expectedError {
				t.Fatalf("expected error %s, got %v", testcase.expectedError, err)
			}

			if diff := cmp.Diff(updatedNRT, testcase.expectedUpdatedNRT); diff != "" {
				t.Fatalf("unexpected updated NRT (-want +got):\n%s", diff)
			}
		})
	}
}

// getTestNRT is a test helper function that returns the current actual NRT state for a node before any dry-run preemption.
// The main interest is the resources accounting for the node, not any other attributes.
func getTestNRT() *topologyv1alpha2.NodeResourceTopology {
	return &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Zones: []topologyv1alpha2.Zone{
			{
				Name: "node-0",
				Resources: []topologyv1alpha2.ResourceInfo{
					{
						Name:        "cpu",
						Capacity:    resource.MustParse("10"),
						Allocatable: resource.MustParse("10"),
						Available:   resource.MustParse("1"),
					},
					{
						Name:        "memory",
						Capacity:    resource.MustParse("500Mi"),
						Allocatable: resource.MustParse("500Mi"),
						Available:   resource.MustParse("100Mi"),
					},
					{
						Name:        "example-device.com/deviceA",
						Capacity:    resource.MustParse("8"),
						Allocatable: resource.MustParse("8"),
						Available:   resource.MustParse("1"),
					},
				},
			},
			{
				Name: "node-1",
				Resources: []topologyv1alpha2.ResourceInfo{
					{
						Name:        "cpu",
						Capacity:    resource.MustParse("10"),
						Allocatable: resource.MustParse("10"),
						Available:   resource.MustParse("1"),
					},
					{
						Name:        "memory",
						Capacity:    resource.MustParse("500Mi"),
						Allocatable: resource.MustParse("500Mi"),
						Available:   resource.MustParse("100Mi"),
					},
					{
						Name:        "example-device.com/deviceB",
						Capacity:    resource.MustParse("8"),
						Allocatable: resource.MustParse("8"),
						Available:   resource.MustParse("2"),
					},
				},
			},
		},
	}
}

// getTestEncodedInfo10Containers returns a *numaplacement.EncodedInfo populated with 10
// containers spread across 2 NUMA nodes.
func getTestEncodedInfo10Containers() *numaplacement.EncodedInfo {
	affinities := []numaplacement.ContainerAffinity{
		{ID: numaplacement.ContainerID{Namespace: "ns-a", PodName: "pod-0", ContainerName: "cnt-0"}, NUMANode: 0},
		{ID: numaplacement.ContainerID{Namespace: "ns-a", PodName: "pod-0", ContainerName: "cnt-1"}, NUMANode: 0},
		{ID: numaplacement.ContainerID{Namespace: "ns-a", PodName: "pod-0", ContainerName: "cnt-3"}, NUMANode: 0},

		{ID: numaplacement.ContainerID{Namespace: "ns-a", PodName: "pod-a-1", ContainerName: "cnt-0"}, NUMANode: 0},

		{ID: numaplacement.ContainerID{Namespace: "ns-a", PodName: "pod-a-2", ContainerName: "cnt-0"}, NUMANode: 0},

		{ID: numaplacement.ContainerID{Namespace: "ns-b", PodName: "pod-0", ContainerName: "cnt-0"}, NUMANode: 1},
		{ID: numaplacement.ContainerID{Namespace: "ns-b", PodName: "pod-0", ContainerName: "cnt-1"}, NUMANode: 1},
		{ID: numaplacement.ContainerID{Namespace: "ns-b", PodName: "pod-0", ContainerName: "cnt-3"}, NUMANode: 1},

		{ID: numaplacement.ContainerID{Namespace: "ns-b", PodName: "pod-1", ContainerName: "cnt-0"}, NUMANode: 1},
		{ID: numaplacement.ContainerID{Namespace: "ns-b", PodName: "pod-2", ContainerName: "cnt-0"}, NUMANode: 1},
	}

	enc, err := numaplacement.NewEncoder(2, affinities...)
	if err != nil {
		panic("getTestEncodedInfo10Containers: encoder: " + err.Error())
	}
	pl, err := enc.Result()
	if err != nil {
		panic("getTestEncodedInfo10Containers: encoder result: " + err.Error())
	}

	ids := make([]numaplacement.ContainerID, len(affinities))
	for i, ca := range affinities {
		ids[i] = ca.ID
	}
	dec, err := numaplacement.NewDecoder(pl, ids...)
	if err != nil {
		panic("getTestEncodedInfo10Containers: decoder: " + err.Error())
	}
	info, err := dec.Result()
	if err != nil {
		panic("getTestEncodedInfo10Containers: decoder result: " + err.Error())
	}

	encInfo, ok := info.(*numaplacement.EncodedInfo)
	if !ok {
		panic("getTestEncodedInfo10Containers: unexpected Info concrete type")
	}
	return encInfo
}
