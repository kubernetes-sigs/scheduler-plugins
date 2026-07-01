/*
Copyright 2026 The Kubernetes Authors.

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

package noderesourcetopology

import (
	"context"
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/numaplacement"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
)

type fakeFilterCache struct {
	nrt               *topologyv1alpha2.NodeResourceTopology
	numaPlacement     *numaplacement.EncodedInfo
	maybeOverReserved []string
}

func (f *fakeFilterCache) GetCachedNRTCopy(_ context.Context, _ string, _ *v1.Pod) (*topologyv1alpha2.NodeResourceTopology, nrtcache.CachedNRTInfo) {
	if f.nrt == nil {
		return nil, nrtcache.CachedNRTInfo{Fresh: true}
	}
	return f.nrt.DeepCopy(), nrtcache.CachedNRTInfo{Fresh: true, Generation: 1}
}

func (f *fakeFilterCache) GetNUMAPlacementInfo(_ string) *numaplacement.EncodedInfo {
	return f.numaPlacement
}

func (f *fakeFilterCache) NodeMaybeOverReserved(nodeName string, _ *v1.Pod) {
	f.maybeOverReserved = append(f.maybeOverReserved, nodeName)
}

func (f *fakeFilterCache) NodeHasForeignPods(string, *v1.Pod)     {}
func (f *fakeFilterCache) ReserveNodeResources(string, *v1.Pod)   {}
func (f *fakeFilterCache) UnreserveNodeResources(string, *v1.Pod) {}
func (f *fakeFilterCache) PostBind(string, *v1.Pod)               {}

func makeGuaranteedPod(namespace, name, containerName string, cpu int64, memory string) *v1.Pod {
	req := v1.ResourceList{
		v1.ResourceCPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
		v1.ResourceMemory: resource.MustParse(memory),
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Name: containerName,
				Resources: v1.ResourceRequirements{
					Requests: req,
					Limits:   req,
				},
			}},
		},
	}
}

func makeTopologyResInfoWithAllocatable(name, capacity, available string) topologyv1alpha2.ResourceInfo {
	qty := resource.MustParse(capacity)
	return topologyv1alpha2.ResourceInfo{
		Name:        name,
		Capacity:    qty,
		Allocatable: qty,
		Available:   resource.MustParse(available),
	}
}

func makePreemptionNRT(nodeName string) *topologyv1alpha2.NodeResourceTopology {
	return &topologyv1alpha2.NodeResourceTopology{
		ObjectMeta:       metav1.ObjectMeta{Name: nodeName},
		TopologyPolicies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
		Zones: topologyv1alpha2.ZoneList{
			{
				Name: "node-0",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					makeTopologyResInfoWithAllocatable(cpu, "4", "0"),
					makeTopologyResInfoWithAllocatable(memory, "8Gi", "7Gi"),
				},
			},
			{
				Name: "node-1",
				Type: "Node",
				Resources: topologyv1alpha2.ResourceInfoList{
					makeTopologyResInfoWithAllocatable(cpu, "4", "2"),
					makeTopologyResInfoWithAllocatable(memory, "8Gi", "8Gi"),
				},
			},
		},
	}
}

func makeNodeFromNRT(nrt *topologyv1alpha2.NodeResourceTopology) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nrt.Name},
		Status: v1.NodeStatus{
			Capacity:    makeResourceListFromZones(nrt.Zones),
			Allocatable: makeResourceListFromZones(nrt.Zones),
		},
	}
}

func makeEncodedInfoForPod(pod *v1.Pod, numaNode int) *numaplacement.EncodedInfo {
	affinities := make([]numaplacement.ContainerAffinity, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		affinities = append(affinities, numaplacement.ContainerAffinity{
			ID: numaplacement.ContainerID{
				Namespace:     pod.Namespace,
				PodName:       pod.Name,
				ContainerName: container.Name,
			},
			NUMANode: numaNode,
		})
	}

	enc, err := numaplacement.NewEncoder(2, affinities...)
	if err != nil {
		panic(err)
	}
	payload, err := enc.Result()
	if err != nil {
		panic(err)
	}

	ids := make([]numaplacement.ContainerID, len(affinities))
	for i, affinity := range affinities {
		ids[i] = affinity.ID
	}
	dec, err := numaplacement.NewDecoder(payload, ids...)
	if err != nil {
		panic(err)
	}
	info, err := dec.Result()
	if err != nil {
		panic(err)
	}
	encoded, ok := info.(*numaplacement.EncodedInfo)
	if !ok {
		panic("unexpected numaplacement info type")
	}
	return encoded
}

func cycleStateWithVictims(t *testing.T, victims ...*v1.Pod) fwk.CycleState {
	t.Helper()
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()
	if _, status := tm.PreFilter(context.Background(), cycleState, nil, nil); !status.IsSuccess() {
		t.Fatalf("PreFilter failed: %v", status)
	}
	for _, victim := range victims {
		if status := tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim), nil); !status.IsSuccess() {
			t.Fatalf("RemovePod failed for %s/%s: %v", victim.Namespace, victim.Name, status)
		}
	}
	return cycleState
}

func TestFilter_PreemptionFlow(t *testing.T) {
	const nodeName = "node-preempt"
	const victimNS = "default"
	const victimName = "victim"

	nrt := makePreemptionNRT(nodeName)
	node := makeNodeFromNRT(nrt)
	victim := makeGuaranteedPod(victimNS, victimName, containerName, 4, "1Gi")
	numaPlacement := makeEncodedInfoForPod(victim, 0)

	preemptor := makeGuaranteedPod("default", "preemptor", containerName, 4, "1Gi")

	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)

	t.Run("without preemption node is unschedulable", func(t *testing.T) {
		cache := &fakeFilterCache{nrt: nrt, numaPlacement: numaPlacement}
		tm := TopologyMatch{nrtCache: cache}

		status := tm.Filter(context.Background(), framework.NewCycleState(), preemptor, nodeInfo)
		if !quasiEqualStatus(status, fwk.NewStatus(fwk.Unschedulable, "cannot align container")) {
			t.Fatalf("expected unschedulable without preemption, got %v", status)
		}
		if len(cache.maybeOverReserved) != 1 || cache.maybeOverReserved[0] != nodeName {
			t.Fatalf("expected node marked over-reserved, got %v", cache.maybeOverReserved)
		}
	})

	t.Run("victim eviction makes node schedulable", func(t *testing.T) {
		cache := &fakeFilterCache{nrt: nrt, numaPlacement: numaPlacement}
		tm := TopologyMatch{nrtCache: cache}
		cycleState := cycleStateWithVictims(t, victim)

		status := tm.Filter(context.Background(), cycleState, preemptor, nodeInfo)
		if !quasiEqualStatus(status, nil) {
			t.Fatalf("expected schedulable with victim eviction, got %v", status)
		}
		if len(cache.maybeOverReserved) != 0 {
			t.Fatalf("preemption flow must not mark node over-reserved, got %v", cache.maybeOverReserved)
		}
	})

	t.Run("victims without NUMA placement info stay unschedulable", func(t *testing.T) {
		cache := &fakeFilterCache{nrt: nrt}
		tm := TopologyMatch{nrtCache: cache}
		cycleState := cycleStateWithVictims(t, victim)

		status := tm.Filter(context.Background(), cycleState, preemptor, nodeInfo)
		if !quasiEqualStatus(status, fwk.NewStatus(fwk.Unschedulable, "cannot align container")) {
			t.Fatalf("expected unschedulable without NUMA placement info, got %v", status)
		}
		if len(cache.maybeOverReserved) != 0 {
			t.Fatalf("preemption flow must not mark node over-reserved, got %v", cache.maybeOverReserved)
		}
	})

	t.Run("preemption with unschedulable result skips over-reserve marking", func(t *testing.T) {
		cache := &fakeFilterCache{nrt: nrt}
		tm := TopologyMatch{nrtCache: cache}
		cycleState := cycleStateWithVictims(t, victim)

		tooLarge := makeGuaranteedPod("default", "too-large", containerName, 8, "1Gi")
		status := tm.Filter(context.Background(), cycleState, tooLarge, nodeInfo)
		if !quasiEqualStatus(status, fwk.NewStatus(fwk.Unschedulable, "cannot align container")) {
			t.Fatalf("expected unschedulable preemptor, got %v", status)
		}
		if len(cache.maybeOverReserved) != 0 {
			t.Fatalf("preemption flow must not mark node over-reserved on failure, got %v", cache.maybeOverReserved)
		}
	})
}

func TestGetVictimPods(t *testing.T) {
	victim := makeGuaranteedPod("default", "victim", containerName, 1, "1Gi")
	cycleState := cycleStateWithVictims(t, victim)

	got, err := getVictimPods(cycleState)
	if err != nil {
		t.Fatalf("getVictimPods failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 victim, got %d", len(got))
	}
	if got[0].Name != victim.Name || got[0].Namespace != victim.Namespace {
		t.Fatalf("unexpected victim: %+v", got[0])
	}
}
