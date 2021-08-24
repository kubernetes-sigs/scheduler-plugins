/*
Copyright 2021 The Kubernetes Authors.

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
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"gonum.org/v1/gonum/stat"
)

func balancedAllocationScoreStrategy(requested, allocatable v1.ResourceList, resourceToWeightMap resourceToWeightMap) int64 {
	resourceFractions := make([]float64, 0)

	// We don't care what kind of resources are being requested, we just iterate all of them.
	// If NUMA zone doesn't have the requested resource, the score for that resource will be 0.
	for resourceName := range requested {
		resourceFraction := fractionOfCapacity(requested[resourceName], allocatable[resourceName])
		// if requested > capacity the corresponding NUMA zone should never be preferred
		if resourceFraction > 1 {
			return 0
		}
		resourceFractions = append(resourceFractions, resourceFraction)
	}

	variance := stat.Variance(resourceFractions, nil)

	// Since the variance is between positive fractions, it will be positive fraction. 1-variance lets the
	// score to be higher for node which has least variance and multiplying it with `MaxNodeScore` provides the scaling
	// factor needed.
	return int64((1 - variance) * float64(framework.MaxNodeScore))
}

func fractionOfCapacity(requested, capacity resource.Quantity) float64 {
	if capacity.Value() == 0 {
		return 1
	}
	return float64(requested.Value()) / float64(capacity.Value())
}
