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
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/numaplacement"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"

	"github.com/k8stopologyawareschedwg/podfingerprint"
)

func TestFingerprintFromNRT(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		TopologyPolicies: []string{
			"best-effort",
		},
	}

	pfpTestAnn := "test-ann"
	pfpTestAttr := "test-attr"

	tcases := []struct {
		description string
		anns        map[string]string
		attrs       []topologyv1alpha2.AttributeInfo
		expectedPFP string
	}{
		{
			description: "no anns, attr",
			expectedPFP: "",
		},
		{
			description: "no attrs, empty anns",
			anns:        map[string]string{},
			expectedPFP: "",
		},
		{
			description: "no attrs, empty pfp ann",
			anns: map[string]string{
				podfingerprint.Annotation: "",
			},
			expectedPFP: "",
		},
		{
			description: "no attrs, pfp ann",
			anns: map[string]string{
				podfingerprint.Annotation: pfpTestAnn,
			},
			expectedPFP: pfpTestAnn,
		},
		{
			description: "attr overrides, pfp ann",
			anns: map[string]string{
				podfingerprint.Annotation: pfpTestAnn,
			},
			attrs: []topologyv1alpha2.AttributeInfo{
				{
					Name:  podfingerprint.Attribute,
					Value: pfpTestAttr,
				},
			},
			expectedPFP: pfpTestAttr,
		},
		{
			description: "attr, no ann",
			attrs: []topologyv1alpha2.AttributeInfo{
				{
					Name:  podfingerprint.Attribute,
					Value: pfpTestAttr,
				},
			},
			expectedPFP: pfpTestAttr,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			nrtObj := nrt.DeepCopy()
			if tcase.anns != nil {
				nrtObj.Annotations = make(map[string]string)
				for key, value := range tcase.anns {
					nrtObj.Annotations[key] = value
				}
			}
			for _, attr := range tcase.attrs {
				nrtObj.Attributes = append(nrtObj.Attributes, attr)
			}
			pfp, _ := podFingerprintForNodeTopology(nrtObj, apiconfig.CacheResyncAutodetect)
			if pfp != tcase.expectedPFP {
				t.Errorf("misdetected fingerprint as %q expected %q (anns=%v attrs=%v)", pfp, tcase.expectedPFP, nrtObj.Annotations, nrtObj.Attributes)
			}
		})
	}
}

func TestFingerprintMethodFromNRT(t *testing.T) {
	pfpTestAttr := "test-attr"
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		TopologyPolicies: []string{
			"best-effort",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  podfingerprint.Attribute,
				Value: pfpTestAttr,
			},
		},
	}

	tcases := []struct {
		description         string
		methodValue         string
		expectedOnlyExclRes bool
	}{
		{
			description:         "no attr",
			methodValue:         "",
			expectedOnlyExclRes: false,
		},
		{
			description:         "unrecognized attr",
			methodValue:         "foobar",
			expectedOnlyExclRes: false,
		},
		{
			description:         "all (old default)",
			methodValue:         podfingerprint.MethodAll,
			expectedOnlyExclRes: false,
		},
		{
			description:         "exclusive only",
			methodValue:         podfingerprint.MethodWithExclusiveResources,
			expectedOnlyExclRes: true,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			nrtObj := nrt.DeepCopy()
			if tcase.methodValue != "" {
				nrtObj.Attributes = append(nrt.Attributes, topologyv1alpha2.AttributeInfo{
					Name:  podfingerprint.AttributeMethod,
					Value: tcase.methodValue,
				})
			}

			_, onlyExclRes := podFingerprintForNodeTopology(nrtObj, apiconfig.CacheResyncAutodetect)
			if onlyExclRes != tcase.expectedOnlyExclRes {
				t.Errorf("misdetected method: expected %v (from %q) got %v", tcase.expectedOnlyExclRes, tcase.methodValue, onlyExclRes)
			}
		})
	}
}

func TestNRTStoreGet(t *testing.T) {
	nrts := []topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-0",
			},
			TopologyPolicies: []string{
				"best-effort",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
			},
			TopologyPolicies: []string{
				"restricted",
			},
		},
	}
	ns := newNrtStore(klog.Background(), nrts)

	obj := ns.GetNRTCopyByNodeName("node-0")
	obj.TopologyPolicies[0] = "single-numa-node"

	obj2 := ns.GetNRTCopyByNodeName("node-0")
	if obj2.TopologyPolicies[0] != nrts[0].TopologyPolicies[0] {
		t.Errorf("change to local copy propagated back in the store")
	}

	nrts[0].TopologyPolicies[0] = "single-numa-node"
	obj2 = ns.GetNRTCopyByNodeName("node-0")
	if obj2.TopologyPolicies[0] != "best-effort" { // original value when the object was first added to the store
		t.Errorf("stored value is not an independent copy")
	}
}

func TestNRTStoreUpdate(t *testing.T) {
	nrts := []topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-0",
			},
			TopologyPolicies: []string{
				"best-effort",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
			},
			TopologyPolicies: []string{
				"restricted",
			},
		},
	}
	ns := newNrtStore(klog.Background(), nrts)

	nrt3 := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-2",
		},
		TopologyPolicies: []string{
			"none",
		},
	}
	ns.Update(nrt3)
	nrt3.TopologyPolicies[0] = "best-effort"

	obj3 := ns.GetNRTCopyByNodeName("node-2")
	if obj3.TopologyPolicies[0] != "none" { // original value when the object was first added to the store
		t.Errorf("stored value is not an independent copy")
	}
}

func TestNRTStoreUpdateWithPods(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  numaplacement.AttributeMetadata,
				Value: "npv0v001::ve=leb89::cc=3::nn=2::bn=1",
			},
			{
				Name:  nodeconfig.AttributePolicy,
				Value: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			},
		},
		Zones: topologyv1alpha2.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Attributes: []topologyv1alpha2.AttributeInfo{
					{
						Name:  numaplacement.AttributeVector,
						Value: "!", // this is the encoding that tells cnt1 is on this NUMA
					},
				},
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "20", "20"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "20", "20"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
					MakeTopologyResInfo(nicName, "8", "8"),
				},
			},
		},
	}
	cnt0 := numaplacement.ContainerID{Namespace: "ns-0", PodName: "pod-0", ContainerName: "container-0"}
	cnt1 := numaplacement.ContainerID{Namespace: "ns-0", PodName: "pod-1", ContainerName: "container-1"}
	cnt2 := numaplacement.ContainerID{Namespace: "ns-0", PodName: "pod-1", ContainerName: "container-2"}
	pods := []podData{
		{
			Namespace:                        "ns-0",
			Name:                             "pod-0",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{cnt0},
		},
		{
			Namespace:                        "ns-0",
			Name:                             "pod-1",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{cnt1, cnt2},
		},
		{
			Namespace: "ns-1",
			Name:      "pod-2",
		},
	}

	ns := newNrtStore(klog.Background(), []topologyv1alpha2.NodeResourceTopology{*nrt})
	ns.Update(nrt, pods...)
	updatedNUMAPlacement := ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if updatedNUMAPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	expectedContainers := 3
	if updatedNUMAPlacement.Containers() != expectedContainers {
		t.Errorf("unexpected number of containers: got %d expected %d", updatedNUMAPlacement.Containers(), expectedContainers)
	}

	// check that the NUMAPlacement info is for the expected containers
	hostingNUMAForCnt0, err := updatedNUMAPlacement.NUMAAffinity(cnt0)
	if err != nil {
		t.Fatalf("failed to get NUMA affinity for container %s: %v", cnt0.String(), err)
	}
	if hostingNUMAForCnt0 != 1 {
		t.Errorf("container %s is hosted on NUMA %d, expected %d", cnt0.String(), hostingNUMAForCnt0, 1)
	}

	hostingNUMAForCnt1, err := updatedNUMAPlacement.NUMAAffinity(cnt1)
	if err != nil {
		t.Fatalf("failed to get NUMA affinity for container %s: %v", cnt1.String(), err)
	}
	if hostingNUMAForCnt1 != 0 {
		t.Errorf("container %s is hosted on NUMA %d, expected %d", cnt1.String(), hostingNUMAForCnt1, 0)
	}

	hostingNUMAForCnt2, err := updatedNUMAPlacement.NUMAAffinity(cnt2)
	if err != nil {
		t.Fatalf("failed to get NUMA affinity for container %s: %v", cnt2.String(), err)
	}
	if hostingNUMAForCnt2 != 1 {
		t.Errorf("container %s is hosted on NUMA %d, expected %d", cnt2, hostingNUMAForCnt2, 1)
	}

	// update the NRT and pods such that cnt0 is gone
	pods = pods[1:]
	nrt.Attributes[0].Value = "npv0v001::ve=leb89::cc=2::nn=2::bn=1"
	// mimick removing cnt0 from the node hence removing the vector attribute for that NUMA
	nrt.Zones[0].Attributes = []topologyv1alpha2.AttributeInfo{}
	ns.Update(nrt, pods...)
	updatedNUMAPlacement = ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if updatedNUMAPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	if updatedNUMAPlacement.Containers() != 2 {
		t.Errorf("unexpected number of containers: got %d expected 2", updatedNUMAPlacement.Containers())
	}

	hostingNUMAForCnt0, err = updatedNUMAPlacement.NUMAAffinity(cnt0)
	if err != numaplacement.ErrUnknownContainer {
		t.Fatalf("expected error getting NUMA affinity for no longer existing container %s but got %v", cnt0.String(), err)
	}
	if hostingNUMAForCnt0 != -1 {
		t.Errorf("container %s is hosted on NUMA %d, expected %d", cnt0.String(), hostingNUMAForCnt0, -1)
	}

	hostingNUMAForCnt1, err = updatedNUMAPlacement.NUMAAffinity(cnt1)
	if err != nil {
		t.Fatalf("failed to get NUMA affinity for container %s: %v", cnt1.String(), err)
	}
	if hostingNUMAForCnt1 != 1 {
		t.Errorf("container %s is hosted on NUMA %d, expected 1", cnt1.String(), hostingNUMAForCnt1)
	}

}

func TestNRTStoreUpdateWithoutPods(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  numaplacement.AttributeMetadata,
				Value: "npv0v001::ve=leb89::cc=0::nn=2::bn=0",
			},
			{
				Name:  nodeconfig.AttributePolicy,
				Value: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			},
		},
	}

	ns := newNrtStore(klog.Background(), []topologyv1alpha2.NodeResourceTopology{*nrt})
	ns.Update(nrt) // no pods
	numaPlacement := ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if numaPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	if numaPlacement.Containers() != 0 {
		t.Errorf("unexpected containers in NUMAPlacement: got %d expected %d", numaPlacement.Containers(), 0)
	}
}

func TestNRTStoreUpdateNoNUMAPlacementDueToPolicy(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  nodeconfig.AttributePolicy,
				Value: "best-effort", // not single-numa-node
			},
		},
	}
	pods := []podData{
		{
			Namespace: "ns-0",
			Name:      "pod-0",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{
				{Namespace: "ns-0", PodName: "pod-0", ContainerName: "container-0"},
			},
		},
	}

	ns := newNrtStore(klog.Background(), []topologyv1alpha2.NodeResourceTopology{*nrt})
	ns.Update(nrt, pods...)
	numaPlacement := ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if numaPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	if numaPlacement.Containers() != 0 {
		t.Errorf("unexpected containers in NUMAPlacement: got %d expected %d", numaPlacement.Containers(), 0)
	}
}

func TestNRTStoreUpdateNoNUMAPlacementDueToMissingMetadata(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  nodeconfig.AttributePolicy,
				Value: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			},
			// no metadata attribute
		},
	}
	pods := []podData{
		{
			Namespace: "ns-0",
			Name:      "pod-0",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{
				{Namespace: "ns-0", PodName: "pod-0", ContainerName: "container-0"},
			},
		},
	}

	ns := newNrtStore(klog.Background(), []topologyv1alpha2.NodeResourceTopology{*nrt})
	ns.Update(nrt, pods...)
	numaPlacement := ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if numaPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	if numaPlacement.Containers() != 0 {
		t.Errorf("unexpected containers in NUMAPlacement: got %d expected %d", numaPlacement.Containers(), 0)
	}
}

func TestNRTStoreUpdateNoNUMAPlacementDueToInconsistentContainersCount(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-0",
		},
		Attributes: []topologyv1alpha2.AttributeInfo{
			{
				Name:  numaplacement.AttributeMetadata,
				Value: "npv0v001::ve=leb89::cc=5::nn=2::bn=1", // claims 5 containers
			},
			{
				Name:  nodeconfig.AttributePolicy,
				Value: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			},
		},
	}
	pods := []podData{
		{
			Namespace: "ns-0",
			Name:      "pod-0",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{
				{Namespace: "ns-0", PodName: "pod-0", ContainerName: "container-0"},
				{Namespace: "ns-0", PodName: "pod-0", ContainerName: "container-1"},
			},
		},
		{
			Namespace: "ns-0",
			Name:      "pod-1",
			ContainersWithExclusiveResources: []numaplacement.ContainerID{
				{Namespace: "ns-0", PodName: "pod-1", ContainerName: "container-0"},
			},
		},
		// 3 exclusive containers total
	}

	ns := newNrtStore(klog.Background(), []topologyv1alpha2.NodeResourceTopology{*nrt})
	ns.Update(nrt, pods...)
	numaPlacement := ns.GetNUMAPlacementInfoByNodeName(nrt.Name)
	if numaPlacement == nil {
		t.Fatalf("missing NUMAPlacement info after update")
	}
	if numaPlacement.Containers() != 0 {
		t.Errorf("unexpected containers in NUMAPlacement: got %d expected %d", numaPlacement.Containers(), 0)
	}
}

func TestNRTStoreGetMissing(t *testing.T) {
	ns := newNrtStore(klog.Background(), nil)
	if ns.GetNRTCopyByNodeName("node-missing") != nil {
		t.Errorf("missing node returned non-nil data")
	}
}

func TestNRTStoreContains(t *testing.T) {
	ns := newNrtStore(klog.Background(), nil)
	if ns.Contains("node-0") {
		t.Errorf("unexpected node found")
	}

	nrts := []topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-0",
			},
			TopologyPolicies: []string{
				"best-effort",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
			},
			TopologyPolicies: []string{
				"restricted",
			},
		},
	}
	ns = newNrtStore(klog.Background(), nrts)
	if !ns.Contains("node-0") {
		t.Errorf("missing node")
	}
}

func TestCounterIncr(t *testing.T) {
	cnt := newCounter()

	if cnt.IsSet("missing") {
		t.Errorf("found nonexisting key in empty counter")
	}

	cnt.Incr("aaa")
	cnt.Incr("aaa")
	if val := cnt.Incr("aaa"); val != 3 {
		t.Errorf("unexpected counter value: %d expected %d", val, 3)
	}
	cnt.Incr("bbb")

	if !cnt.IsSet("aaa") {
		t.Errorf("missing expected key: %q", "aaa")
	}
	if !cnt.IsSet("bbb") {
		t.Errorf("missing expected key: %q", "bbb")
	}
}

func TestCounterDelete(t *testing.T) {
	cnt := newCounter()

	cnt.Incr("aaa")
	cnt.Incr("aaa")
	cnt.Incr("bbb")

	cnt.Delete("aaa")
	if cnt.IsSet("aaa") {
		t.Errorf("found unexpected key: %q", "aaa")
	}
	if !cnt.IsSet("bbb") {
		t.Errorf("missing expected key: %q", "bbb")
	}
}

func TestCounterKeys(t *testing.T) {
	cnt := newCounter()

	cnt.Incr("a")
	cnt.Incr("b")
	cnt.Incr("c")
	cnt.Incr("b")
	cnt.Incr("a")
	cnt.Incr("c")

	keys := cnt.Keys()
	sort.Strings(keys)
	expected := []string{"a", "b", "c"}
	if !reflect.DeepEqual(keys, expected) {
		t.Errorf("keys mismatch got=%v expected=%v", keys, expected)
	}
}

func TestCounterClone(t *testing.T) {
	cnt := newCounter()

	cnt.Incr("a")
	cnt.Incr("b")
	cnt.Incr("c")

	cnt2 := cnt.Clone()
	cnt2.Incr("d")

	if cnt.IsSet("d") {
		t.Errorf("imperfect clone, key \"d\" set on cloned")
	}
	if !cnt2.IsSet("d") {
		t.Errorf("imperfect clone, key \"d\" NOT set on clone")
	}
}

func TestCounterLen(t *testing.T) {
	cnt := newCounter()
	val := cnt.Len()
	if val != 0 {
		t.Errorf("unexpected len: %d", val)
	}

	cnt.Incr("a")
	cnt.Incr("b")
	cnt.Incr("a")
	val = cnt.Len()
	if val != 2 {
		t.Errorf("unexpected len: %d", val)
	}
}

const (
	nicName = "vendor_A.com/nic"
)

func TestResourceStoreAddPod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-0",
			Name:      "pod-0",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "cnt-0",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("16"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}

	rs := newResourceStore(klog.Background())
	existed := rs.AddPod(&pod)
	if existed {
		t.Fatalf("replaced a pod into a empty resourceStore")
	}
	existed = rs.AddPod(&pod)
	if !existed {
		t.Fatalf("added pod twice")
	}
}

func TestResourceStoreDeletePod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-0",
			Name:      "pod-0",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "cnt-0",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("16"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}

	rs := newResourceStore(klog.Background())
	existed := rs.DeletePod(&pod)
	if existed {
		t.Fatalf("deleted a pod into a empty resourceStore")
	}
	rs.AddPod(&pod)
	existed = rs.DeletePod(&pod)
	if !existed {
		t.Fatalf("deleted a pod which was not supposed to be present")
	}
}

func TestResourceStoreUpdate(t *testing.T) {
	nrt := &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta:       metav1.ObjectMeta{Name: "node"},
		TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodePodLevel)},
		Zones: topologyv1alpha2.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "20", "20"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					MakeTopologyResInfo(cpu, "20", "20"),
					MakeTopologyResInfo(memory, "32Gi", "32Gi"),
					MakeTopologyResInfo(nicName, "8", "8"),
				},
			},
		},
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-0",
			Name:      "pod-0",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "cnt-0",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:           resource.MustParse("16"),
							corev1.ResourceMemory:        resource.MustParse("4Gi"),
							corev1.ResourceName(nicName): resource.MustParse("2"),
						},
					},
				},
				{
					Name: "cnt-1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
	}

	rs := newResourceStore(klog.Background())
	existed := rs.AddPod(&pod)
	if existed {
		t.Fatalf("replacing a pod into a empty resourceStore")
	}

	logID := "testResourceStoreUpdate"
	rs.UpdateNRT(nrt, "logID", logID)

	cpuInfo0 := findResourceInfo(nrt.Zones[0].Resources, cpu)
	if cpuInfo0.Capacity.Cmp(resource.MustParse("20")) != 0 {
		t.Errorf("bad capacity for resource %q on zone %d: expected %v got %v", cpu, 0, "20", cpuInfo0.Capacity)
	}
	if cpuInfo0.Available.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("bad availability for resource %q on zone %d: expected %v got %v", cpu, 0, "2", cpuInfo0.Available)
	}
	cpuInfo1 := findResourceInfo(nrt.Zones[1].Resources, cpu)
	if cpuInfo1.Capacity.Cmp(resource.MustParse("20")) != 0 {
		t.Errorf("bad capacity for resource %q on zone %d: expected %v got %v", cpu, 1, "20", cpuInfo1.Capacity)
	}
	if cpuInfo1.Available.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("bad availability for resource %q on zone %d: expected %v got %v", cpu, 1, "2", cpuInfo1.Available)
	}

	memInfo0 := findResourceInfo(nrt.Zones[0].Resources, memory)
	if memInfo0.Capacity.Cmp(resource.MustParse("32Gi")) != 0 {
		t.Errorf("bad capacity for resource %q on zone %d: expected %v got %v", memory, 0, "32Gi", memInfo0.Capacity)
	}
	if memInfo0.Available.Cmp(resource.MustParse("26Gi")) != 0 {
		t.Errorf("bad availability for resource %q on zone %d: expected %v got %v", memory, 0, "26Gi", memInfo0.Available)
	}
	memInfo1 := findResourceInfo(nrt.Zones[1].Resources, memory)
	if memInfo1.Capacity.Cmp(resource.MustParse("32Gi")) != 0 {
		t.Errorf("bad capacity for resource %q on zone %d: expected %v got %v", memory, 1, "32Gi", memInfo1.Capacity)
	}
	if memInfo1.Available.Cmp(resource.MustParse("26Gi")) != 0 {
		t.Errorf("bad availability for resource %q on zone %d: expected %v got %v", memory, 1, "26Gi", memInfo1.Available)
	}

	devInfo0 := findResourceInfo(nrt.Zones[0].Resources, nicName)
	if devInfo0 != nil {
		t.Errorf("unexpected device %q on zone %d", nicName, 0)
	}

	devInfo1 := findResourceInfo(nrt.Zones[1].Resources, nicName)
	if devInfo1 == nil {
		t.Fatalf("expected device %q on zone %d, but missing", nicName, 1)
	}
	if devInfo1.Capacity.Cmp(resource.MustParse("8")) != 0 {
		t.Errorf("bad capacity for resource %q on zone %d: expected %v got %v", nicName, 1, "8", devInfo1.Capacity)
	}
	if devInfo1.Available.Cmp(resource.MustParse("6")) != 0 {
		t.Errorf("bad availability for resource %q on zone %d: expected %v got %v", nicName, 1, "6", devInfo1.Available)
	}
}

func TestCheckPodFingerprintForNode(t *testing.T) {
	tcases := []struct {
		description string
		objs        []podData
		onlyExclRes bool
		pfp         string
		expectedErr error
	}{
		{
			description: "nil objs",
			onlyExclRes: false,
			expectedErr: podfingerprint.ErrMalformed,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			gotErr := checkPodFingerprintForNode(klog.Background(), tcase.objs, "test-node", tcase.pfp, tcase.onlyExclRes)
			if !errors.Is(gotErr, tcase.expectedErr) {
				t.Errorf("got error %v expected %v", gotErr, tcase.expectedErr)
			}
		})
	}
}

func findResourceInfo(rinfos []topologyv1alpha2.ResourceInfo, name string) *topologyv1alpha2.ResourceInfo {
	for idx := 0; idx < len(rinfos); idx++ {
		if rinfos[idx].Name == name {
			return &rinfos[idx]
		}
	}
	return nil
}

type fakePodLister struct {
	pods []*corev1.Pod
	err  error
}

type fakePodNamespaceLister struct {
	parent    *fakePodLister
	namespace string
}

func (fpl *fakePodLister) AddPod(pod *corev1.Pod) {
	fpl.pods = append(fpl.pods, pod)
}

func (fpl *fakePodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return fpl.pods, fpl.err
}

func (fpl *fakePodLister) Pods(namespace string) podlisterv1.PodNamespaceLister {
	return &fakePodNamespaceLister{
		parent:    fpl,
		namespace: namespace,
	}
}

func (fpnl *fakePodNamespaceLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return nil, fmt.Errorf("not yet implemented")
}

func (fpnl *fakePodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	return nil, fmt.Errorf("not yet implemented")
}
