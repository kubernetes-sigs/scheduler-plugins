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
	"context"
	"reflect"
	"sort"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/podfingerprint"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	podlisterv1 "k8s.io/client-go/listers/core/v1"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

const (
	cpu             = string(corev1.ResourceCPU)
	memory          = string(corev1.ResourceMemory)
	nicResourceName = "vendor.com/nic1"
)

func TestGetCacheResyncMethod(t *testing.T) {
	resyncAutodetect := apiconfig.CacheResyncAutodetect
	resyncAll := apiconfig.CacheResyncAll
	resyncOnlyExclusiveResources := apiconfig.CacheResyncOnlyExclusiveResources

	testCases := []struct {
		description string
		cfg         *apiconfig.NodeResourceTopologyCache
		expected    apiconfig.CacheResyncMethod
	}{
		{
			description: "nil config",
			expected:    apiconfig.CacheResyncAutodetect,
		},
		{
			description: "empty config",
			cfg:         &apiconfig.NodeResourceTopologyCache{},
			expected:    apiconfig.CacheResyncAutodetect,
		},
		{
			description: "explicit all",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ResyncMethod: &resyncAll,
			},
			expected: apiconfig.CacheResyncAll,
		},
		{
			description: "explicit autodetect",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ResyncMethod: &resyncAutodetect,
			},
			expected: apiconfig.CacheResyncAutodetect,
		},
		{
			description: "explicit OnlyExclusiveResources",
			cfg: &apiconfig.NodeResourceTopologyCache{
				ResyncMethod: &resyncOnlyExclusiveResources,
			},
			expected: apiconfig.CacheResyncOnlyExclusiveResources,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			got := getCacheResyncMethod(testCase.cfg)
			if got != testCase.expected {
				t.Errorf("cache resync method got %v expected %v", got, testCase.expected)
			}
		})
	}
}
func TestInitEmptyLister(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	_, err = NewOverReserve(nil, nil, fakePodLister)
	if err == nil {
		t.Fatalf("accepted nil lister")
	}

	_, err = NewOverReserve(nil, fakeClient, nil)
	if err == nil {
		t.Fatalf("accepted nil indexer")
	}
}

func TestNodesMaybeOverReservedCount(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)
	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}
}

func TestDirtyNodesMarkDiscarded(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	expectedNodes := []string{
		"node-1",
		"node-4",
	}

	for _, nodeName := range expectedNodes {
		nrtCache.ReserveNodeResources(nodeName, &corev1.Pod{})
	}

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}

	for _, nodeName := range expectedNodes {
		nrtCache.NodeMaybeOverReserved(nodeName, &corev1.Pod{})
	}

	dirtyNodes = nrtCache.NodesMaybeOverReserved("testing")
	sort.Strings(dirtyNodes)

	if !reflect.DeepEqual(dirtyNodes, expectedNodes) {
		t.Errorf("got=%v expected=%v", dirtyNodes, expectedNodes)
	}
}

func TestDirtyNodesUnmarkedOnReserve(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	availNodes := []string{
		"node-1",
		"node-4",
	}

	for _, nodeName := range availNodes {
		nrtCache.ReserveNodeResources(nodeName, &corev1.Pod{})
	}

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}

	for _, nodeName := range availNodes {
		nrtCache.NodeMaybeOverReserved(nodeName, &corev1.Pod{})
	}

	// assume noe update which unblocks node-4
	nrtCache.ReserveNodeResources("node-4", &corev1.Pod{})

	expectedNodes := []string{
		"node-1",
	}

	dirtyNodes = nrtCache.NodesMaybeOverReserved("testing")

	if !reflect.DeepEqual(dirtyNodes, expectedNodes) {
		t.Errorf("got=%v expected=%v", dirtyNodes, expectedNodes)
	}
}

func TestOverreserveGetCachedNRTCopy(t *testing.T) {
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
			expectedNRT:    nil,
			expectedOK:     false,
		},
	}

	checkGetCachedNRTCopy(
		t,
		func(client ctrlclient.Client, podLister podlisterv1.PodLister) (Interface, error) {
			return NewOverReserve(nil, client, podLister)
		},
		testCases...,
	)
}

func TestGetCachedNRTCopyReserve(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}
	nrtCache.ReserveNodeResources("node1", testPod)

	nrtObj, _ := nrtCache.GetCachedNRTCopy(context.Background(), "node1", testPod)
	for _, zone := range nrtObj.Zones {
		for _, zoneRes := range zone.Resources {
			switch zoneRes.Name {
			case string(corev1.ResourceCPU):
				if zoneRes.Available.Cmp(resource.MustParse("22")) != 0 {
					t.Errorf("quantity mismatch in zone %q", zoneRes.Name)
				}
			case string(corev1.ResourceMemory):
				if zoneRes.Available.Cmp(resource.MustParse("44Gi")) != 0 {
					t.Errorf("quantity mismatch in zone %q", zoneRes.Name)
				}
			}
		}
	}
}

func TestGetCachedNRTCopyReleaseNone(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}
	nrtCache.UnreserveNodeResources("node1", testPod)

	nrtObj, _ := nrtCache.GetCachedNRTCopy(context.Background(), "node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestGetCachedNRTCopyReserveRelease(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}
	nrtCache.ReserveNodeResources("node1", testPod)
	nrtCache.UnreserveNodeResources("node1", testPod)

	nrtObj, _ := nrtCache.GetCachedNRTCopy(context.Background(), "node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestFlush(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}

	logID := "testFlush"

	nrtCache.ReserveNodeResources("node1", testPod)
	nrtCache.NodeMaybeOverReserved("node1", testPod)

	expectedNodeTopology := &topologyv1alpha2.NodeResourceTopology{
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
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	nrtCache.FlushNodes(logID, expectedNodeTopology.DeepCopy())

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes after flush: %v", dirtyNodes)
	}

	nrtObj, _ := nrtCache.GetCachedNRTCopy(context.Background(), "node1", testPod)
	if !reflect.DeepEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestResyncNoPodFingerprint(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "namespace1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}
	nrtCache.ReserveNodeResources("node1", testPod)
	nrtCache.NodeMaybeOverReserved("node1", testPod)

	expectedNodeTopology := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
		},
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
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	fakeClient.Create(context.Background(), expectedNodeTopology)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")

	if len(dirtyNodes) != 1 || dirtyNodes[0] != "node1" {
		t.Errorf("cleaned nodes after resyncing with bad data: %v", dirtyNodes)
	}
}

func TestResyncMatchFingerprint(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := makeDefaultTestTopology()
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "namespace1",
		},
		Spec: corev1.PodSpec{
			NodeName: "node1",
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
					},
				},
			},
		},
	}
	nrtCache.ReserveNodeResources("node1", testPod)
	nrtCache.NodeMaybeOverReserved("node1", testPod)

	expectedNodeTopology := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				podfingerprint.Annotation: "pfp0v0019e0420efb37746c6",
			},
		},
		Attributes: topologyv1alpha2.AttributeList{
			{
				Name:  podfingerprint.Attribute,
				Value: "pfp0v0019e0420efb37746c6",
			},
		},
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
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	runningPod := testPod.DeepCopy()
	runningPod.Status.Phase = corev1.PodRunning

	if err := fakeClient.Create(context.Background(), expectedNodeTopology); err != nil {
		t.Fatal(err)
	}
	fakePodLister.AddPod(runningPod)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) > 0 {
		t.Errorf("node still dirty after resyncing with good data: %v", dirtyNodes)
	}

	nrtObj, _ := nrtCache.GetCachedNRTCopy(context.Background(), "node1", testPod)
	if !isNRTEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected nrt from cache\ngot: %v\nexpected: %v\n",
			dumpNRT(nrtObj), dumpNRT(expectedNodeTopology))
	}
}

func isNRTEqual(a, b *topologyv1alpha2.NodeResourceTopology) bool {
	return equality.Semantic.DeepDerivative(a.Zones, b.Zones) &&
		equality.Semantic.DeepDerivative(a.TopologyPolicies, b.TopologyPolicies) &&
		equality.Semantic.DeepDerivative(a.Attributes, b.Attributes)
}

func TestUnknownNodeWithForeignPods(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nrtCache.NodeHasForeignPods("node-bogus", &corev1.Pod{})

	names := nrtCache.NodesMaybeOverReserved("testing")
	if len(names) != 0 {
		t.Errorf("non-existent node has foreign pods!")
	}
}

func TestNodeWithForeignPods(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)

	nodeTopologies := []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node1-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node1-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
			},
		},
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node2"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node2-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node2-1",
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
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	target := "node2"
	nrtCache.NodeHasForeignPods(target, &corev1.Pod{})

	names := nrtCache.NodesMaybeOverReserved("testing")
	if len(names) != 1 || names[0] != target {
		t.Errorf("unexpected dirty nodes: %v", names)
	}

	_, ok := nrtCache.GetCachedNRTCopy(context.Background(), target, &corev1.Pod{})
	if ok {
		t.Errorf("succesfully got node with foreign pods!")
	}
}

func mustOverReserve(t *testing.T, client ctrlclient.Client, podLister podlisterv1.PodLister) *OverReserve {
	obj, err := NewOverReserve(nil, client, podLister)
	if err != nil {
		t.Fatalf("unexpected error creating cache: %v", err)
	}
	return obj
}
