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
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	faketopologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"
	listerv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/podfingerprint"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
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
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	var err error
	_, err = NewOverReserve(nil, nil, fakePodLister)
	if err == nil {
		t.Fatalf("accepted nil lister")
	}

	_, err = NewOverReserve(nil, fakeInformer.Lister(), nil)
	if err == nil {
		t.Fatalf("accepted nil indexer")
	}
}

func TestNodesMaybeOverReservedCount(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)
	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}
}

func TestDirtyNodesMarkDiscarded(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

func TestGetCachedNRTCopy(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

	var nrtObj *topologyv1alpha2.NodeResourceTopology
	nrtObj, _ = nrtCache.GetCachedNRTCopy("node1", &corev1.Pod{})
	if nrtObj != nil {
		t.Fatalf("non-empty object from empty cache")
	}

	nodeTopologies := []*topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha2.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "4"),
						MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "10"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha2.ResourceInfoList{
						MakeTopologyResInfo(cpu, "30", "8"),
						MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "10"),
					},
				},
			},
		},
	}
	for _, obj := range nodeTopologies {
		nrtCache.Store().Update(obj)
	}

	nrtObj, _ = nrtCache.GetCachedNRTCopy("node1", &corev1.Pod{})
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestGetCachedNRTCopyReserve(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	nrtObj, _ := nrtCache.GetCachedNRTCopy("node1", testPod)
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
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	nrtObj, _ := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestGetCachedNRTCopyReserveRelease(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	nrtObj, _ := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestFlush(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	nrtObj, _ := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestResyncNoPodFingerprint(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	fakeInformer.Informer().GetStore().Add(expectedNodeTopology)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")

	if len(dirtyNodes) != 1 || dirtyNodes[0] != "node1" {
		t.Errorf("cleaned nodes after resyncing with bad data: %v", dirtyNodes)
	}
}

func TestResyncMatchFingerprint(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	fakeInformer.Informer().GetStore().Add(expectedNodeTopology)
	fakePodLister.AddPod(runningPod)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved("testing")
	if len(dirtyNodes) > 0 {
		t.Errorf("node still dirty after resyncing with good data: %v", dirtyNodes)
	}

	nrtObj, _ := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestUnknownNodeWithForeignPods(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

	nrtCache.NodeHasForeignPods("node-bogus", &corev1.Pod{})

	names := nrtCache.NodesMaybeOverReserved("testing")
	if len(names) != 0 {
		t.Errorf("non-existent node has foreign pods!")
	}
}

func TestNodeWithForeignPods(t *testing.T) {
	fakeClient := faketopologyv1alpha2.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha2().NodeResourceTopologies()
	fakePodLister := &fakePodLister{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakePodLister)

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

	_, ok := nrtCache.GetCachedNRTCopy(target, &corev1.Pod{})
	if ok {
		t.Errorf("succesfully got node with foreign pods!")
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

func mustOverReserve(t *testing.T, nrtLister listerv1alpha2.NodeResourceTopologyLister, podLister podlisterv1.PodLister) *OverReserve {
	obj, err := NewOverReserve(nil, nrtLister, podLister)
	if err != nil {
		t.Fatalf("unexpected error creating cache: %v", err)
	}
	return obj
}
