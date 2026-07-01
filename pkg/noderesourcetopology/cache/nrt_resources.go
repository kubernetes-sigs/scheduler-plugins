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

package cache

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

// ResourceNamesFromNRT returns the set of resource names listed in the given NRT zones.
func ResourceNamesFromNRT(nrt *topologyv1alpha2.NodeResourceTopology) sets.Set[corev1.ResourceName] {
	if nrt == nil {
		return nil
	}
	res := sets.New[corev1.ResourceName]()
	for _, zone := range nrt.Zones {
		for _, resInfo := range zone.Resources {
			res.Insert(corev1.ResourceName(resInfo.Name))
		}
	}
	return res
}
