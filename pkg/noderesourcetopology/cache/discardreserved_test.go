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
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDiscardReservedNodesGetCachedNRTCopy(t *testing.T) {
	testNodeName := "worker-node-1"
	nrt := makeTestNRT(testNodeName)

	testCases := []testCaseGetCachedNRTCopy{
		{
			name: "data present with foreign pods",
			nodeTopologies: []*topologyv1alpha2.NodeResourceTopology{
				nrt,
			},
			nodeName:       testNodeName,
			hasForeignPods: true,
			expectedNRT:    nrt,
			expectedOK:     true,
		},
	}

	checkGetCachedNRTCopy(
		t,
		func(client ctrlclient.WithWatch, _ podlisterv1.PodLister) (Interface, error) {
			return NewDiscardReserved(klog.Background(), client), nil
		},
		testCases...,
	)
}

func TestDiscardReservedNodesGetNRTCopyFails(t *testing.T) {
	nrtCache := DiscardReserved{
		reservationMap: map[string]map[types.UID]bool{
			"node1": {
				types.UID("some-uid"): true,
			},
		},
	}

	nrtObj, nrtInfo := nrtCache.GetCachedNRTCopy(context.Background(), "node1", &corev1.Pod{})
	if nrtInfo.Fresh {
		t.Fatal("expected false\ngot true\n")
	}
	if nrtObj != nil {
		t.Fatalf("non-empty object")
	}
}

func TestDiscardReservedNodesReserveNodeResources(t *testing.T) {
	nrtCache := DiscardReserved{
		reservationMap: make(map[string]map[types.UID]bool),
	}

	nrtCache.ReserveNodeResources("node1", &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "test",
			UID:       "some-uid",
		},
	})
	nodePods, ok := nrtCache.reservationMap["node1"]
	if !ok {
		t.Fatal("expected reservationMap to have entry for node1")
	}

	if len(nodePods) != 1 {
		t.Fatalf("expected reservationMap entry for node1 to have len 1 not: %d", len(nodePods))
	}

	pod, ok := nodePods[types.UID("some-uid")]
	if !ok {
		t.Fatal("expected reservationMap for node1 to have some-uid")
	}

	if !pod {
		t.Fatal("expected reservationMap entry node1 to have some-uid set to true not false")
	}
}

func TestDiscardReservedNodesRemoveReservationForNode(t *testing.T) {
	nrtCache := DiscardReserved{
		reservationMap: make(map[string]map[types.UID]bool),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "test",
			UID:       "some-uid",
		},
	}

	nrtCache.ReserveNodeResources("node1", pod)
	nodePods, ok := nrtCache.reservationMap["node1"]
	if !ok {
		t.Fatal("expected reservationMap to have entry for node1")
	}

	if len(nodePods) != 1 {
		t.Fatalf("expected reservationMap entry for node1 to have len 1 not: %d", len(nodePods))
	}

	podExists, ok := nodePods[types.UID("some-uid")]
	if !ok {
		t.Fatal("expected reservationMap for node1 to have some-uid")
	}

	if !podExists {
		t.Fatal("expected reservationMap entry node1 to have some-uid set to true not false")
	}

	nrtCache.removeReservationForNode("node1", pod)
	nodePods, ok = nrtCache.reservationMap["node1"]
	if !ok {
		t.Fatal("expected reservationMap to have entry for node1")
	}

	if len(nodePods) != 0 {
		t.Fatalf("expected reservationMap entry for node1 to have len 0 not: %d", len(nodePods))
	}
}
