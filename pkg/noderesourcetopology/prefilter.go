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
	"fmt"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

const stateVictimPodsKey fwk.StateKey = "stateVictimPods"

type PodsInfo map[string]*v1.Pod // namespace/podname -> pod

func (pi PodsInfo) GetPods() []v1.Pod {
	pods := make([]v1.Pod, 0, len(pi))
	for _, pod := range pi {
		pods = append(pods, *pod)
	}
	return pods
}

// PreemptionStack implements the fwk.StateData interface for prefilter.
type PreemptionStack struct {
	PodsToRemove PodsInfo
}

func (s *PreemptionStack) Clone() fwk.StateData {
	cloned := make(PodsInfo, len(s.PodsToRemove))
	for podID, pod := range s.PodsToRemove {
		cloned[podID] = pod.DeepCopy()
	}
	return &PreemptionStack{
		PodsToRemove: cloned,
	}
}

// The extension point is implemented only to be able to use AddPod/RemovePod functions during preemption flow
func (tm *TopologyMatch) PreFilter(ctx context.Context, cycleState fwk.CycleState, _ *v1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	cycleState.Write(stateVictimPodsKey, &PreemptionStack{
		PodsToRemove: make(PodsInfo),
	})
	return nil, fwk.NewStatus(fwk.Success, "")
}

func (tm *TopologyMatch) PreFilterExtensions() fwk.PreFilterExtensions {
	return tm
}

// AddPod is called by the framework while trying to evaluate the impact
// of adding podToAdd to the node while scheduling podToSchedule. In other
// words, this removes the pod from the preemption stack.
func (tm *TopologyMatch) AddPod(ctx context.Context, cycleState fwk.CycleState, podToSchedule *v1.Pod, podInfoToAdd fwk.PodInfo, _ fwk.NodeInfo) *fwk.Status {
	pod := podInfoToAdd.GetPod()
	if pod == nil {
		return fwk.NewStatus(fwk.Error, "missing pod")
	}
	ps, err := readPreemptionStack(cycleState)
	if err != nil {
		return fwk.NewStatus(fwk.Error, err.Error())
	}

	delete(ps.PodsToRemove, pod.Namespace+"/"+pod.Name)
	cycleState.Write(stateVictimPodsKey, ps)
	return nil
}

// RemovePod is called by the framework while trying to evaluate the impact
// of removing podToRemove from the node while scheduling podToSchedule.
func (tm *TopologyMatch) RemovePod(ctx context.Context, cycleState fwk.CycleState, podToSchedule *v1.Pod, podInfoToRemove fwk.PodInfo, _ fwk.NodeInfo) *fwk.Status {
	pod := podInfoToRemove.GetPod()
	if pod == nil {
		return fwk.NewStatus(fwk.Error, "missing pod")
	}
	ps, err := readPreemptionStack(cycleState)
	if err != nil {
		return fwk.NewStatus(fwk.Error, err.Error())
	}

	ps.PodsToRemove[pod.Namespace+"/"+pod.Name] = pod
	cycleState.Write(stateVictimPodsKey, ps)
	return nil
}

func readPreemptionStack(state fwk.CycleState) (*PreemptionStack, error) {
	data, err := state.Read(stateVictimPodsKey)
	if err != nil {
		return nil, err
	}
	s, ok := data.(*PreemptionStack)
	if !ok {
		return nil, fmt.Errorf("cannot convert saved state to preFilterState")
	}
	return s, nil
}
