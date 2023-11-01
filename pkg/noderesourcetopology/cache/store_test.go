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

	"github.com/google/go-cmp/cmp"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"

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
	ns := newNrtStore(nrts)

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
	ns := newNrtStore(nrts)

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

func TestNRTStoreGetMissing(t *testing.T) {
	ns := newNrtStore(nil)
	if ns.GetNRTCopyByNodeName("node-missing") != nil {
		t.Errorf("missing node returned non-nil data")
	}
}

func TestNRTStoreContains(t *testing.T) {
	ns := newNrtStore(nil)
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
	ns = newNrtStore(nrts)
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

	rs := newResourceStore()
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

	rs := newResourceStore()
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

	rs := newResourceStore()
	existed := rs.AddPod(&pod)
	if existed {
		t.Fatalf("replacing a pod into a empty resourceStore")
	}

	logID := "testResourceStoreUpdate"
	rs.UpdateNRT(logID, nrt)

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

func TestMakeNodeToPodDataMap(t *testing.T) {
	tcases := []struct {
		description string
		pods        []*corev1.Pod
		err         error
		expected    map[string][]podData
		expectedErr error
	}{
		{
			description: "empty pod list",
			expected:    make(map[string][]podData),
		},
		{
			description: "single pod NOT running",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace1",
						Name:      "pod1",
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
				},
			},
			expected: make(map[string][]podData),
		},
		{
			description: "single pod running",
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
			description: "few pods, single node running",
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
			got, err := makeNodeToPodDataMap(podLister, tcase.description)
			if err != tcase.expectedErr {
				t.Errorf("error mismatch: got %v expected %v", err, tcase.expectedErr)
			}
			if diff := cmp.Diff(got, tcase.expected); diff != "" {
				t.Errorf("unexpected result: %v", diff)
			}
		})
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
			gotErr := checkPodFingerprintForNode("testing", tcase.objs, "test-node", tcase.pfp, tcase.onlyExclRes)
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
