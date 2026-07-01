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
	"maps"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func makeTestPod(namespace, name string, containers ...string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
	for _, c := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{Name: c})
	}
	return pod
}

func makeTestPodInfo(pod *v1.Pod) fwk.PodInfo {
	pi, _ := framework.NewPodInfo(pod)
	return pi
}

func TestClone(t *testing.T) {
	original := &PreemptionStack{
		PodsToRemove: PodsInfo{
			"pod-a": makeTestPod("ns", "pod-a", "c1"),
		},
	}

	cloned := original.Clone().(*PreemptionStack)

	storedPod, ok := cloned.PodsToRemove["pod-a"]
	if !ok {
		t.Fatal("cloned state missing pod-a")
	}
	if storedPod.Name != "pod-a" {
		t.Fatalf("cloned pod name mismatch: got %s, want pod-a", storedPod.Name)
	}

	delete(cloned.PodsToRemove, "pod-a")
	if _, ok := original.PodsToRemove["pod-a"]; !ok {
		t.Fatal("deleting from clone should not affect original")
	}
}

func TestPreFilter(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()

	result, status := tm.PreFilter(context.Background(), cycleState, nil, nil)
	if !status.IsSuccess() {
		t.Fatalf("PreFilter returned non-success: %v", status)
	}
	if result != nil {
		t.Fatalf("PreFilter should return nil result, got %v", result)
	}

	ps, err := readPreemptionStack(cycleState)
	if err != nil {
		t.Fatalf("failed to read PreemptionStack after PreFilter: %v", err)
	}
	if ps.PodsToRemove == nil {
		t.Fatal("PodsToRemove map should be initialized")
	}
	if len(ps.PodsToRemove) != 0 {
		t.Fatalf("PodsToRemove should be empty, got %d entries", len(ps.PodsToRemove))
	}
}

func TestRemovePod_WithoutPreFilter(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()

	victim := makeTestPod("ns", "victim", "c1")

	status := tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim), nil)
	if status.IsSuccess() {
		t.Fatal("RemovePod should fail when PreFilter was not called (no state in CycleState)")
	}
	if status.Code() != fwk.Error {
		t.Fatalf("expected Error status, got %v", status.Code())
	}
}

func TestRemovePod(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()
	tm.PreFilter(context.Background(), cycleState, nil, nil)

	victim1 := makeTestPod("ns", "victim-1", "c1")
	victim2 := makeTestPod("ns", "victim-2", "c2")
	victim3 := makeTestPod("ns", "victim-3", "c3")

	tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim1), nil)
	tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim2), nil)
	tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim3), nil)

	ps, _ := readPreemptionStack(cycleState)
	if len(ps.PodsToRemove) != 3 {
		t.Fatalf("expected 3 victim pods, got %d", len(ps.PodsToRemove))
	}
	if _, ok := ps.PodsToRemove["ns/victim-1"]; !ok {
		t.Error("victim-1 missing")
	}
	if _, ok := ps.PodsToRemove["ns/victim-2"]; !ok {
		t.Error("victim-2 missing")
	}
	if _, ok := ps.PodsToRemove["ns/victim-3"]; !ok {
		t.Error("victim-3 missing")
	}
}

func TestAddPod_WithoutPreFilter(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()

	victim := makeTestPod("ns", "victim", "c1")

	status := tm.AddPod(context.Background(), cycleState, nil, makeTestPodInfo(victim), nil)
	if status.IsSuccess() {
		t.Fatal("AddPod should fail when PreFilter was not called (no state in CycleState)")
	}
	if status.Code() != fwk.Error {
		t.Fatalf("expected Error status, got %v", status.Code())
	}
}

func TestAddPod(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()
	tm.PreFilter(context.Background(), cycleState, makeTestPod("ns", "scheduler"), nil)

	victim1 := makeTestPod("ns", "victim-1", "c1")
	victim2 := makeTestPod("ns", "victim-2", "c2")

	tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim1), nil)
	tm.RemovePod(context.Background(), cycleState, nil, makeTestPodInfo(victim2), nil)

	status := tm.AddPod(context.Background(), cycleState, nil, makeTestPodInfo(victim1), nil)
	if !status.IsSuccess() {
		t.Fatalf("AddPod returned non-success: %v", status)
	}

	ps, _ := readPreemptionStack(cycleState)
	if _, ok := ps.PodsToRemove["ns/victim-1"]; ok {
		t.Error("victim-1 should be deleted after reprieve")
	}
	if _, ok := ps.PodsToRemove["ns/victim-2"]; !ok {
		t.Error("victim-2 should not be deleted after reprieve")
	}
}

func TestAddPod_EmptyStack(t *testing.T) {
	tm := &TopologyMatch{}
	cycleState := framework.NewCycleState()
	tm.PreFilter(context.Background(), cycleState, makeTestPod("ns", "scheduler"), nil)

	psBefore, err := readPreemptionStack(cycleState)
	if err != nil {
		t.Fatalf("failed to read PreemptionStack before AddPod: %v", err)
	}
	victim1 := makeTestPod("ns", "victim-1", "c1")
	status := tm.AddPod(context.Background(), cycleState, nil, makeTestPodInfo(victim1), nil)
	if !status.IsSuccess() {
		t.Fatalf("AddPod returned non-success: %v", status)
	}

	psAfter, err := readPreemptionStack(cycleState)
	if err != nil {
		t.Fatalf("failed to read PreemptionStack after AddPod: %v", err)
	}
	if !maps.Equal(psBefore.PodsToRemove, psAfter.PodsToRemove) {
		t.Fatalf("expected same PreemptionStack after AddPod to empty stack")
	}
}

func TestGetPods(t *testing.T) {
	podA := makeTestPod("ns", "pod-a", "c1")
	podB := makeTestPod("ns", "pod-b", "c2", "c3")
	podC := makeTestPod("other", "pod-c")
	testcases := []struct {
		name         string
		pi           PodsInfo
		expectedPods []v1.Pod
	}{
		{
			name:         "empty",
			pi:           PodsInfo{},
			expectedPods: []v1.Pod{},
		},
		{
			name: "single pod",
			pi: PodsInfo{
				"pod-a": podA,
			},
			expectedPods: []v1.Pod{*podA},
		},
		{
			name: "multiple pods",
			pi: PodsInfo{
				"pod-a": podA,
				"pod-b": podB,
				"pod-c": podC,
			},
			expectedPods: []v1.Pod{*podA, *podB, *podC},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			pods := tc.pi.GetPods()
			if len(pods) != len(tc.expectedPods) {
				t.Fatalf("expected %d pods, got %d", len(tc.expectedPods), len(pods))
			}

			if diff := cmp.Diff(pods, tc.expectedPods); diff != "" {
				t.Fatalf("pods mismatch: %s", diff)
			}
		})
	}
}
