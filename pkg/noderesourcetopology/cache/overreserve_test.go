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

	"github.com/google/go-cmp/cmp"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/podfingerprint"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/podprovider"
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
			got := getCacheResyncMethod(klog.Background(), testCase.cfg)
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
	ctx := context.Background()
	_, err = NewOverReserve(ctx, klog.Background(), nil, nil, fakePodLister, podprovider.IsPodRelevantAlways)
	if err == nil {
		t.Fatalf("accepted nil lister")
	}

	_, err = NewOverReserve(ctx, klog.Background(), nil, fakeClient, nil, podprovider.IsPodRelevantAlways)
	if err == nil {
		t.Fatalf("accepted nil indexer")
	}
}

func TestGetDesyncedNodesCount(t *testing.T) {
	fakeClient, err := tu.NewFakeClient()
	if err != nil {
		t.Fatal(err)
	}

	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeClient, fakePodLister)
	dirtyNodes := nrtCache.GetDesyncedNodes(klog.Background())
	if dirtyNodes.DirtyCount() != 0 {
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

	dirtyNodes := nrtCache.GetDesyncedNodes(klog.Background())
	if dirtyNodes.Len() != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}

	for _, nodeName := range expectedNodes {
		nrtCache.NodeMaybeOverReserved(nodeName, &corev1.Pod{})
	}

	dirtyNodes = nrtCache.GetDesyncedNodes(klog.Background())
	sort.Strings(dirtyNodes.MaybeOverReserved)

	if !reflect.DeepEqual(dirtyNodes.MaybeOverReserved, expectedNodes) {
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

	dirtyNodes := nrtCache.GetDesyncedNodes(klog.Background())
	if dirtyNodes.Len() != 0 {
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

	dirtyNodes = nrtCache.GetDesyncedNodes(klog.Background())

	if !reflect.DeepEqual(dirtyNodes.MaybeOverReserved, expectedNodes) {
		t.Errorf("got=%v expected=%v", dirtyNodes.MaybeOverReserved, expectedNodes)
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
		func(client ctrlclient.WithWatch, podLister podlisterv1.PodLister) (Interface, error) {
			return NewOverReserve(context.Background(), klog.Background(), nil, client, podLister, podprovider.IsPodRelevantAlways)
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

	lh := klog.Background()

	nrtCache.FlushNodes(lh, expectedNodeTopology.DeepCopy())

	dirtyNodes := nrtCache.GetDesyncedNodes(lh)
	if dirtyNodes.Len() != 0 {
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

	dirtyNodes := nrtCache.GetDesyncedNodes(klog.Background())

	if dirtyNodes.Len() != 1 || dirtyNodes.MaybeOverReserved[0] != "node1" {
		t.Errorf("cleaned nodes after resyncing with bad data: %v", dirtyNodes.MaybeOverReserved)
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

	dirtyNodes := nrtCache.GetDesyncedNodes(klog.Background())
	if dirtyNodes.Len() > 0 {
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

	nodes := nrtCache.GetDesyncedNodes(klog.Background())
	if nodes.Len() != 0 {
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

	nodes := nrtCache.GetDesyncedNodes(klog.Background())
	if nodes.Len() != 1 || nodes.MaybeOverReserved[0] != target {
		t.Errorf("unexpected dirty nodes: %v", nodes.MaybeOverReserved)
	}

	_, info := nrtCache.GetCachedNRTCopy(context.Background(), target, &corev1.Pod{})
	if info.Fresh {
		t.Errorf("succesfully got node with foreign pods!")
	}
}

func mustOverReserve(t *testing.T, client ctrlclient.WithWatch, podLister podlisterv1.PodLister) *OverReserve {
	obj, err := NewOverReserve(context.Background(), klog.Background(), nil, client, podLister, podprovider.IsPodRelevantAlways)
	if err != nil {
		t.Fatalf("unexpected error creating cache: %v", err)
	}
	return obj
}

func TestMakeNodeToPodDataMap(t *testing.T) {
	tcases := []struct {
		description   string
		pods          []*corev1.Pod
		isPodRelevant podprovider.PodFilterFunc
		err           error
		expected      map[string][]podData
		expectedErr   error
	}{
		{
			description:   "empty pod list - shared",
			isPodRelevant: podprovider.IsPodRelevantShared,
			expected:      make(map[string][]podData),
		},
		{
			description:   "empty pod list - dedicated",
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected:      make(map[string][]podData),
		},
		{
			description: "single pod NOT running - succeeded (kubernetes jobs) - dedicated",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected: map[string][]podData{
				"node1": {
					{
						Namespace: "namespace1",
						Name:      "pod1",
					},
				},
			},
		},
		{
			description: "single pod NOT running - failed",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected: map[string][]podData{
				"node1": {
					{
						Namespace: "namespace1",
						Name:      "pod1",
					},
				},
			},
		},
		{
			description: "single pod NOT running - succeeded (kubernetes jobs) - shared",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantShared,
			expected:      map[string][]podData{},
		},
		{
			description: "single pod NOT running - failed - shared",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantShared,
			expected:      map[string][]podData{},
		},
		{
			description: "single pod running - dedicated",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected: map[string][]podData{
				"node1": {
					{
						Namespace: "namespace1",
						Name:      "pod1",
					},
				},
			},
		},
		{
			description: "single pod running - shared",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected: map[string][]podData{
				"node1": {
					{
						Namespace: "namespace1",
						Name:      "pod1",
					},
				},
			},
		},
		{
			description: "few pods, single node running - dedicated",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace2",
						Name:      "pod2",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace2",
						Name:      "pod3",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
			},
			isPodRelevant: podprovider.IsPodRelevantDedicated,
			expected: map[string][]podData{
				"node1": {
					{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					{
						Namespace: "namespace2",
						Name:      "pod2",
					},
					{
						Namespace: "namespace2",
						Name:      "pod3",
					},
				},
			},
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			podLister := &fakePodLister{
				pods: tcase.pods,
				err:  tcase.err,
			}
			got, err := makeNodeToPodDataMap(klog.Background(), podLister, tcase.isPodRelevant)
			if err != tcase.expectedErr {
				t.Errorf("error mismatch: got %v expected %v", err, tcase.expectedErr)
			}
			if diff := cmp.Diff(got, tcase.expected); diff != "" {
				t.Errorf("unexpected result: %v", diff)
			}
		})
	}
}
