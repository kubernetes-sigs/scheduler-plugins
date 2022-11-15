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

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	faketopologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"
	listerv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha1"
	"github.com/k8stopologyawareschedwg/podfingerprint"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	cpu             = string(corev1.ResourceCPU)
	memory          = string(corev1.ResourceMemory)
	nicResourceName = "vendor.com/nic1"
)

func TestInitEmptyLister(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	var err error
	_, err = NewOverReserve(nil, fakeIndex)
	if err == nil {
		t.Fatalf("accepted nil lister")
	}

	_, err = NewOverReserve(fakeInformer.Lister(), nil)
	if err == nil {
		t.Fatalf("accepted nil indexer")
	}
}

func TestNodesMaybeOverReservedCount(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)
	dirtyNodes := nrtCache.NodesMaybeOverReserved()
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}
}

func TestDirtyNodesMarkDiscarded(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

	expectedNodes := []string{
		"node-1",
		"node-4",
	}

	for _, nodeName := range expectedNodes {
		nrtCache.ReserveNodeResources(nodeName, &corev1.Pod{})
	}

	dirtyNodes := nrtCache.NodesMaybeOverReserved()
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes from pristine cache: %v", dirtyNodes)
	}

	for _, nodeName := range expectedNodes {
		nrtCache.NodeMaybeOverReserved(nodeName, &corev1.Pod{})
	}

	dirtyNodes = nrtCache.NodesMaybeOverReserved()
	sort.Strings(dirtyNodes)

	if !reflect.DeepEqual(dirtyNodes, expectedNodes) {
		t.Errorf("got=%v expected=%v", dirtyNodes, expectedNodes)
	}
}

func TestDirtyNodesUnmarkedOnReserve(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

	availNodes := []string{
		"node-1",
		"node-4",
	}

	for _, nodeName := range availNodes {
		nrtCache.ReserveNodeResources(nodeName, &corev1.Pod{})
	}

	dirtyNodes := nrtCache.NodesMaybeOverReserved()
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

	dirtyNodes = nrtCache.NodesMaybeOverReserved()

	if !reflect.DeepEqual(dirtyNodes, expectedNodes) {
		t.Errorf("got=%v expected=%v", dirtyNodes, expectedNodes)
	}
}

func TestGetCachedNRTCopy(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

	var nrtObj *topologyv1alpha1.NodeResourceTopology
	nrtObj = nrtCache.GetCachedNRTCopy("node1", &corev1.Pod{})
	if nrtObj != nil {
		t.Fatalf("non-empty object from empty cache")
	}

	nodeTopologies := []*topologyv1alpha1.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "20", "4"),
						MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						MakeTopologyResInfo(nicResourceName, "30", "10"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
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

	nrtObj = nrtCache.GetCachedNRTCopy("node1", &corev1.Pod{})
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestGetCachedNRTCopyReserve(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	nrtObj := nrtCache.GetCachedNRTCopy("node1", testPod)
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
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	nrtObj := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestGetCachedNRTCopyReserveRelease(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	nrtObj := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, nodeTopologies[0]) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestFlush(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	expectedNodeTopology := &topologyv1alpha1.NodeResourceTopology{
		ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
		TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
		Zones: topologyv1alpha1.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "30"),
					MakeTopologyResInfo(memory, "64Gi", "60Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	nrtCache.FlushNodes(logID, expectedNodeTopology.DeepCopy())

	dirtyNodes := nrtCache.NodesMaybeOverReserved()
	if len(dirtyNodes) != 0 {
		t.Errorf("dirty nodes after flush: %v", dirtyNodes)
	}

	nrtObj := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func TestResyncNoPodFingerprint(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	expectedNodeTopology := &topologyv1alpha1.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
		},
		TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
		Zones: topologyv1alpha1.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "30"),
					MakeTopologyResInfo(memory, "64Gi", "60Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	fakeInformer.Informer().GetStore().Add(expectedNodeTopology)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved()
	if len(dirtyNodes) != 1 || dirtyNodes[0] != "node1" {
		t.Errorf("cleaned nodes after resyncing with bad data: %v", dirtyNodes)
	}
}

func TestResyncMatchFingerprint(t *testing.T) {
	fakeClient := faketopologyv1alpha1.NewSimpleClientset()
	fakeInformer := topologyinformers.NewSharedInformerFactory(fakeClient, 0).Topology().V1alpha1().NodeResourceTopologies()
	fakeIndex := &fakePodByNodeNameIndex{}

	nrtCache := mustOverReserve(t, fakeInformer.Lister(), fakeIndex)

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

	expectedNodeTopology := &topologyv1alpha1.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				podfingerprint.Annotation: "pfp0v0019e0420efb37746c6",
			},
		},
		TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
		Zones: topologyv1alpha1.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "30"),
					MakeTopologyResInfo(memory, "64Gi", "60Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha1.ResourceInfoList{
					MakeTopologyResInfo(cpu, "32", "22"),
					MakeTopologyResInfo(memory, "64Gi", "44Gi"),
					MakeTopologyResInfo(nicResourceName, "16", "16"),
				},
			},
		},
	}

	fakeInformer.Informer().GetStore().Add(expectedNodeTopology)
	fakeIndex.Add(testPod)

	nrtCache.Resync()

	dirtyNodes := nrtCache.NodesMaybeOverReserved()
	if len(dirtyNodes) > 0 {
		t.Errorf("node still dirty after resyncing with good data: %v", dirtyNodes)
	}

	nrtObj := nrtCache.GetCachedNRTCopy("node1", testPod)
	if !reflect.DeepEqual(nrtObj, expectedNodeTopology) {
		t.Fatalf("unexpected object from cache\ngot: %s\nexpected: %s\n", dumpNRT(nrtObj), dumpNRT(nodeTopologies[0]))
	}
}

func dumpNRT(nrtObj *topologyv1alpha1.NodeResourceTopology) string {
	nrtJson, err := json.MarshalIndent(nrtObj, "", " ")
	if err != nil {
		return "marshallingError"
	}
	return string(nrtJson)
}

type fakePodByNodeNameIndex struct {
	pods []*corev1.Pod
	err  error
}

func (fi *fakePodByNodeNameIndex) Add(pod *corev1.Pod) {
	fi.pods = append(fi.pods, pod)
}

func (fi *fakePodByNodeNameIndex) GetPodNamespacedNamesByNode(logID, nodeName string) ([]types.NamespacedName, error) {
	if fi.err != nil {
		return nil, fi.err
	}

	var objs []types.NamespacedName
	for _, pod := range fi.pods {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		objs = append(objs, types.NamespacedName{
			Namespace: pod.GetNamespace(),
			Name:      pod.GetName(),
		})
	}
	return objs, nil
}

func MakeTopologyResInfo(name, capacity, available string) topologyv1alpha1.ResourceInfo {
	return topologyv1alpha1.ResourceInfo{
		Name:      name,
		Capacity:  resource.MustParse(capacity),
		Available: resource.MustParse(available),
	}
}

func makeDefaultTestTopology() []*topologyv1alpha1.NodeResourceTopology {
	return []*topologyv1alpha1.NodeResourceTopology{
		{
			ObjectMeta:       metav1.ObjectMeta{Name: "node1"},
			TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
			Zones: topologyv1alpha1.ZoneList{
				{
					Name: "node-0",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
				{
					Name: "node-1",
					Type: "Node",
					Resources: topologyv1alpha1.ResourceInfoList{
						MakeTopologyResInfo(cpu, "32", "30"),
						MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						MakeTopologyResInfo(nicResourceName, "16", "16"),
					},
				},
			},
		},
	}
}

func mustOverReserve(t *testing.T, lister listerv1alpha1.NodeResourceTopologyLister, indexer NodeIndexer) *OverReserve {
	obj, err := NewOverReserve(lister, indexer)
	if err != nil {
		t.Fatalf("unexpected error creating cache: %v", err)
	}
	return obj
}
