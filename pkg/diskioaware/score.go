/*
Copyright 2024 The Kubernetes Authors.

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

package diskioaware

import (
	"fmt"

	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

type Scorer interface {
	Score(string, *stateData, resource.Handle) (int64, error)
}

func getScorer(scoreStrategy string) (Scorer, error) {
	switch scoreStrategy {
	case string(config.LeastAllocated):
		return &LeastAllocatedScorer{}, nil
	case string(config.MostAllocated):
		return &MostAllocatedScorer{}, nil
	default:
		return nil, fmt.Errorf("unknown score strategy %v", scoreStrategy)
	}
}

type MostAllocatedScorer struct{}

func (scorer *MostAllocatedScorer) Score(node string, state *stateData, rh resource.Handle) (int64, error) {
	if !state.nodeSupportIOI {
		return framework.MaxNodeScore, nil
	}
	ratio, err := rh.(resource.CacheHandle).NodePressureRatio(node, state.request)
	if err != nil {
		return 0, err
	}
	return int64(ratio * float64(framework.MaxNodeScore)), nil
}

type LeastAllocatedScorer struct{}

func (scorer *LeastAllocatedScorer) Score(node string, state *stateData, rh resource.Handle) (int64, error) {
	if !state.nodeSupportIOI {
		return framework.MaxNodeScore, nil
	}
	ratio, err := rh.(resource.CacheHandle).NodePressureRatio(node, state.request)
	if err != nil {
		return 0, err
	}
	return int64((1.0 - ratio) * float64(framework.MaxNodeScore)), nil
}
