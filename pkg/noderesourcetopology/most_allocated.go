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
)

func mostAllocatedScoreStrategy(requested, allocatable v1.ResourceList, resourceToWeightMap resourceToWeightMap) int64 {
	var numaNodeScore int64 = 0
	var weightSum int64 = 0

	for resourceName := range requested {
		// We don't care what kind of resources are being requested, we just iterate all of them.
		// If NUMA zone doesn't have the requested resource, the score for that resource will be 0.
		resourceScore := mostAllocatedScore(requested[resourceName], allocatable[resourceName])
		weight := resourceToWeightMap.weight(resourceName)
		numaNodeScore += resourceScore * weight
		weightSum += weight
	}

	return numaNodeScore / weightSum
}

// The used capacity is calculated on a scale of 0-MaxNodeScore (MaxNodeScore is
// constant with value set to 100).
// 0 being the lowest priority and 100 being the highest.
// The more allocated resources the node has, the higher the score is.
func mostAllocatedScore(requested, numaCapacity resource.Quantity) int64 {
	if numaCapacity.CmpInt64(0) == 0 {
		return 0
	}
	if requested.Cmp(numaCapacity) > 0 {
		return 0
	}

	return requested.Value() * framework.MaxNodeScore / numaCapacity.Value()
}
