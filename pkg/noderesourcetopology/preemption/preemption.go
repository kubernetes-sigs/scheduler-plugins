/*
Copyright The Kubernetes Authors.

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

package preemption

import (
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/numanode"
	"github.com/k8stopologyawareschedwg/numaplacement"
)

// GetNRTPostPodsEviction accumulates the exclusive resources of the victim pods and
// adds them back to the NRT simulating a post-eviction state. Returns an error if
// eviction simulation cannot be performed.
func GetNRTPostPodsEviction(lh logr.Logger, nrt *topologyv1alpha2.NodeResourceTopology, victims []corev1.Pod, numaPlacementInfo *numaplacement.EncodedInfo) (*topologyv1alpha2.NodeResourceTopology, error) {
	if nrt == nil {
		return nil, fmt.Errorf("NRT not found, cannot process eviction simulation")
	}

	if len(victims) == 0 {
		return nrt, fmt.Errorf("no victims found, cannot process eviction simulation")
	}

	if numaPlacementInfo == nil {
		return nrt, fmt.Errorf("numa placement info not found, cannot process eviction simulation")
	}

	if numaPlacementInfo.Containers() == 0 {
		return nrt, fmt.Errorf("no containers found in numa placement info, cannot process eviction simulation")
	}

	nrtResources := cache.ResourceNamesFromNRT(nrt)
	numaToResourcesToAdd, err := accumulateResourcesToAddPerNUMA(victims, numaPlacementInfo, nrtResources)
	if err != nil {
		return nrt, err
	}
	if len(numaToResourcesToAdd) == 0 {
		return nrt, fmt.Errorf("no resources to add, cannot process eviction simulation")
	}
	return addResourcesToNodeResourcesTopology(lh, nrt, numaToResourcesToAdd)
}

func accumulateResourcesToAddPerNUMA(victims []corev1.Pod, numaPlacementInfo *numaplacement.EncodedInfo, nrtResources sets.Set[corev1.ResourceName]) (map[int]corev1.ResourceList, error) {
	numaToResourcesToAdd := make(map[int]corev1.ResourceList) // numaID -> resource list
	for _, victim := range victims {
		// pod level filtering - exit early
		pQos := v1qos.GetPodQOS(&victim)
		if pQos != corev1.PodQOSGuaranteed && !resourcerequests.IncludeNonNative(&victim) {
			continue
		}

		for _, container := range victim.Spec.Containers {
			containerID := numaplacement.ContainerID{
				Namespace:     victim.Namespace,
				PodName:       victim.Name,
				ContainerName: container.Name,
			}
			numaID, err := numaPlacementInfo.NUMAAffinity(containerID)
			if err != nil {
				continue
			}
			if numaID != -1 {
				for resName, resQty := range container.Resources.Requests {
					// resource-level filtering: only add back the exclusive resources
					if !resourcerequests.IsExclusive(pQos, resName, resQty, nrtResources) {
						continue
					}

					numaResources, ok := numaToResourcesToAdd[numaID]
					if !ok {
						numaToResourcesToAdd[numaID] = corev1.ResourceList{
							resName: resQty,
						}
						continue

					}

					currentQty, ok := numaResources[resName]
					if !ok {
						currentQty = resource.Quantity{}
					}
					currentQty.Add(resQty)
					numaToResourcesToAdd[numaID][resName] = currentQty
				}
			}
		}
	}
	return numaToResourcesToAdd, nil
}

func addResourcesToNodeResourcesTopology(lh logr.Logger, nrt *topologyv1alpha2.NodeResourceTopology, numaToResources map[int]corev1.ResourceList) (*topologyv1alpha2.NodeResourceTopology, error) {
	updatedNRT := nrt.DeepCopy()
	// from now on work on the updated NRT
	for zoneIdx, zone := range updatedNRT.Zones {
		numaID, err := numanode.NameToID(zone.Name)
		if err != nil {
			continue
		}

		resListToAdd, ok := numaToResources[numaID]
		if !ok {
			continue
		}

		for resName, resQty := range resListToAdd {
			for resIdx := range zone.Resources {
				// always use a fresh resource reference
				resource := updatedNRT.Zones[zoneIdx].Resources[resIdx].DeepCopy()
				if resource.Name != string(resName) {
					continue
				}

				tmp := resource.Available.DeepCopy()
				tmp.Add(resQty)
				if tmp.Cmp(resource.Allocatable) > 0 {
					lh.V(2).Info("resource release request exceeds NUMA allocatable",
						"zone", zone.Name,
						"numaID", numaID,
						"resource", resName,
						"allocatable", resource.Allocatable,
						"requestToAdd", resQty,
						"postAddAvailable", tmp,
					)
					// one mistake eliminates the whole NRT update for reliability reasons
					return nrt, fmt.Errorf("resource release request exceeds NUMA allocatable")
				}
				updatedNRT.Zones[zoneIdx].Resources[resIdx].Available = tmp
				break
			}
		}
	}
	return updatedNRT, nil
}
