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
		TypeMeta: metav1.TypeMeta{
			Kind:       "NodeResourceTopology",
			APIVersion: "topology.node.k8s.io/v1alpha2",
		},
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
