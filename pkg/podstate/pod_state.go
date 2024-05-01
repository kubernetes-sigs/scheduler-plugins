/*
Copyright 2020 The Kubernetes Authors.

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

package podstate

import (
	"context"
	"fmt"
	"math"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type PodState struct {
	handle framework.Handle
}

var _ = framework.ScorePlugin(&PodState{})

// Name is the name of the plugin used in the Registry and configurations.
const Name = "PodState"

func (ps *PodState) Name() string {
	return Name
}

// Score invoked at the score extension point.
func (ps *PodState) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := ps.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// pe.score favors nodes with terminating pods instead of nominated pods
	// It calculates the sum of the node's terminating pods and nominated pods
	return ps.score(nodeInfo)
}

// ScoreExtensions of the Score plugin.
func (ps *PodState) ScoreExtensions() framework.ScoreExtensions {
	return ps
}

func (ps *PodState) score(nodeInfo *framework.NodeInfo) (int64, *framework.Status) {
	var terminatingPodNum, nominatedPodNum int64
	// get nominated Pods for node from nominatedPodMap
	nominatedPodNum = int64(len(ps.handle.NominatedPodsForNode(nodeInfo.Node().Name)))
	for _, p := range nodeInfo.Pods {
		// Pod is terminating if DeletionTimestamp has been set
		if p.Pod.DeletionTimestamp != nil {
			terminatingPodNum++
		}
	}
	return terminatingPodNum - nominatedPodNum, nil
}

func (ps *PodState) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find highest and lowest scores.
	var highest int64 = -math.MaxInt64
	var lowest int64 = math.MaxInt64
	for _, nodeScore := range scores {
		if nodeScore.Score > highest {
			highest = nodeScore.Score
		}
		if nodeScore.Score < lowest {
			lowest = nodeScore.Score
		}
	}

	// Transform the highest to the lowest score range to fit the framework's min to max node score range.
	oldRange := highest - lowest
	newRange := framework.MaxNodeScore - framework.MinNodeScore
	for i, nodeScore := range scores {
		if oldRange == 0 {
			scores[i].Score = framework.MinNodeScore
		} else {
			scores[i].Score = ((nodeScore.Score - lowest) * newRange / oldRange) + framework.MinNodeScore
		}
	}

	return nil
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &PodState{handle: h}, nil
}
