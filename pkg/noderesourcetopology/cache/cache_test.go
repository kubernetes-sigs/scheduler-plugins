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

package cache

import (
	"context"
	"encoding/json"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	podlisterv1 "k8s.io/client-go/listers/core/v1"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestClonePods(t *testing.T) {
	original := []podData{
		{
			Namespace:          "ns-0",
			Name:               "pod-0",
			PinnedContainers:   []string{"container-0"},
			ExclusiveResources: ExclusiveResourceAlloc,
		},
	}

	cloned := clonePods(original)
	cloned[0].Name = "pod-mutated"
	cloned[0].PinnedContainers[0] = "container-mutated"
	cloned[0].ExclusiveResources = ExclusiveResourceNone
	cloned = append(cloned, podData{Namespace: "ns-0", Name: "pod-1"})

	if len(original) != 1 {
		t.Fatalf("original pods length changed after mutating clone: got %d expected 1", len(original))
	}
	if original[0].Name != "pod-0" {
		t.Errorf("original pod name changed after mutating clone: got %q expected %q", original[0].Name, "pod-0")
	}
	if original[0].PinnedContainers[0] != "container-0" {
		t.Errorf("original pod PinnedContainers changed after mutating clone: got %q expected %q", original[0].PinnedContainers[0], "container-0")
	}
	if original[0].ExclusiveResources != ExclusiveResourceAlloc {
		t.Errorf("original pod ExclusiveResources changed after mutating clone: got %v expected %v", original[0].ExclusiveResources, ExclusiveResourceAlloc)
	}
}

func TestHasExclusiveResources(t *testing.T) {
	testCases := []struct {
		name               string
		exclusiveResources ExclusiveResourceState
		pinnedContainers   []string
		expected           bool
	}{
		{
			name:               "alloc is conclusive regardless of pinned containers",
			exclusiveResources: ExclusiveResourceAlloc,
			pinnedContainers:   nil,
			expected:           true,
		},
		{
			name:               "alloc is conclusive even with pinned containers set",
			exclusiveResources: ExclusiveResourceAlloc,
			pinnedContainers:   []string{"container-0"},
			expected:           true,
		},
		{
			name:               "none is conclusive regardless of pinned containers",
			exclusiveResources: ExclusiveResourceNone,
			pinnedContainers:   nil,
			expected:           false,
		},
		{
			name:               "none is conclusive even with pinned containers set",
			exclusiveResources: ExclusiveResourceNone,
			pinnedContainers:   []string{"container-0"},
			expected:           false,
		},
		{
			name:               "unknown falls back to pinned containers: none present",
			exclusiveResources: ExclusiveResourceUnknown,
			pinnedContainers:   nil,
			expected:           false,
		},
		{
			name:               "unknown falls back to pinned containers: empty slice",
			exclusiveResources: ExclusiveResourceUnknown,
			pinnedContainers:   []string{},
			expected:           false,
		},
		{
			name:               "unknown falls back to pinned containers: present",
			exclusiveResources: ExclusiveResourceUnknown,
			pinnedContainers:   []string{"container-0"},
			expected:           true,
		},
		{
			name:               "zero value podData has no exclusive resources",
			exclusiveResources: ExclusiveResourceUnknown,
			pinnedContainers:   nil,
			expected:           false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pd := podData{
				Namespace:          "ns-0",
				Name:               "pod-0",
				PinnedContainers:   tc.pinnedContainers,
				ExclusiveResources: tc.exclusiveResources,
			}

			got := pd.hasExclusiveResources()
			if got != tc.expected {
				t.Errorf("hasExclusiveResources() = %v, expected %v (exclusiveResources=%v, pinnedContainers=%v)", got, tc.expected, tc.exclusiveResources, tc.pinnedContainers)
			}
		})
	}
}

type testCaseGetCachedNRTCopy struct {
	name           string
	nodeTopologies []*topologyv1alpha2.NodeResourceTopology
	nodeName       string
	hasForeignPods bool
	expectedNRT    *topologyv1alpha2.NodeResourceTopology
	expectedOK     bool
}

func checkGetCachedNRTCopy(t *testing.T, makeCache func(client ctrlclient.WithWatch, podLister podlisterv1.PodLister) (Interface, error), extraCases ...testCaseGetCachedNRTCopy) {
	t.Helper()

	testNodeName := "worker-node-1"
	nrt := makeTestNRT(testNodeName)
	pod := &corev1.Pod{} // API placeholder
	ctx := context.Background()
	fakePodLister := &fakePodLister{}

	testCases := []testCaseGetCachedNRTCopy{
		{
			name:        "empty",
			nodeName:    testNodeName,
			expectedNRT: nil,
			expectedOK:  true, // because there's no data, and the information is fresh
		},
		{
			name: "data present",
			nodeTopologies: []*topologyv1alpha2.NodeResourceTopology{
				nrt,
			},
			nodeName:    testNodeName,
			expectedNRT: nrt,
			expectedOK:  true,
		},
		{
			name: "data missing for node",
			nodeTopologies: []*topologyv1alpha2.NodeResourceTopology{
				nrt,
			},
			nodeName:    "invalid-node",
			expectedNRT: nil,
			expectedOK:  true, // because there's no data, and the information is fresh
		},
	}
	testCases = append(testCases, extraCases...)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.nodeTopologies))
			for _, nrt := range tc.nodeTopologies {
				objs = append(objs, nrt)
			}

			fakeClient, err := tu.NewFakeClient(objs...)
			if err != nil {
				t.Fatal(err)
			}

			nrtCache, err := makeCache(fakeClient, fakePodLister)
			if err != nil {
				t.Fatalf("unexpected error creating cache: %v", err)
			}
			t.Cleanup(nrtCache.Close)

			if tc.hasForeignPods {
				nrtCache.NodeHasForeignPods(tc.nodeName, pod)
			}

			gotNRT, gotInfo := nrtCache.GetCachedNRTCopy(ctx, tc.nodeName, pod)

			if gotInfo.Fresh != tc.expectedOK {
				t.Fatalf("unexpected object status from cache: got: %v expected: %v", gotInfo.Fresh, tc.expectedOK)
			}
			if gotNRT != nil && tc.expectedNRT == nil {
				t.Fatalf("object from cache not nil but expected nil")
			}
			if gotNRT == nil && tc.expectedNRT != nil {
				t.Fatalf("object from cache nil but expected not nil")
			}

			gotJSON := dumpNRT(gotNRT)
			expJSON := dumpNRT(tc.expectedNRT)
			if gotJSON != expJSON {
				t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", gotJSON, expJSON)
			}
		})
	}
}

func makeTestNRT(nodeName string) *topologyv1alpha2.NodeResourceTopology {
	return &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Attributes: topologyv1alpha2.AttributeList{
			{
				Name:  "topologyManagerPolicy",
				Value: "single-numa-node",
			},
			{
				Name:  "topologyManagerScope",
				Value: "container",
			},
		},
		Zones: topologyv1alpha2.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "30"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
					MakeTopologyResInfo(nicResourceName, "8", "8"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "30"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
					MakeTopologyResInfo(nicResourceName, "8", "8"),
				},
			},
		},
	}
}

func dumpNRT(nrtObj *topologyv1alpha2.NodeResourceTopology) string {
	nrtJson, err := json.MarshalIndent(nrtObj, "", " ")
	if err != nil {
		return "marshallingError"
	}
	return string(nrtJson)
}

func MakeTopologyResInfo(name, capacity, available string) topologyv1alpha2.ResourceInfo {
	return topologyv1alpha2.ResourceInfo{
		Name:      name,
		Capacity:  resource.MustParse(capacity),
		Available: resource.MustParse(available),
	}
}

func makeDefaultTestTopology() []*topologyv1alpha2.NodeResourceTopology {
	return []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
			},
		},
	}
}
