package preemption //rethink the name

import (
	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/numanode"
	"github.com/k8stopologyawareschedwg/numaplacement"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
)

// GetNRTPostPodsEviction accumulates the exclusive resources of the victim pods and adds them back to the NRT simulating a post-eviction state.
func GetNRTPostPodsEviction(lh logr.Logger, nrt *topologyv1alpha2.NodeResourceTopology, victims []corev1.Pod, numaPlacementInfo *numaplacement.EncodedInfo) *topologyv1alpha2.NodeResourceTopology {
	numaToResourcesToAdd := accumulateResourcesToAddPerNUMA(victims, numaPlacementInfo)
	return addResourcesToNodeResourcesTopology(lh, nrt, numaToResourcesToAdd)
}

func accumulateResourcesToAddPerNUMA(victims []corev1.Pod, numaPlacementInfo *numaplacement.EncodedInfo) map[int]corev1.ResourceList {
	if numaPlacementInfo == nil || numaPlacementInfo.Containers() == 0 {
		return nil
	}

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
					if !resourcerequests.IsExclusive(pQos, resName, resQty) {
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
	return numaToResourcesToAdd
}

func addResourcesToNodeResourcesTopology(lh logr.Logger, nrt *topologyv1alpha2.NodeResourceTopology, numaToResources map[int]corev1.ResourceList) *topologyv1alpha2.NodeResourceTopology {
	if nrt == nil || len(numaToResources) == 0 {
		return nrt
	}

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
					lh.V(2).Error(nil, "resource release request exceeds NUMA allocatable",
						"zone", zone.Name,
						"numaID", numaID,
						"resource", resName,
						"allocatable", resource.Allocatable.String(),
						"requestToAdd", resQty.String(),
						"postAddAvailable", tmp.String(),
					)
					// one mistake eliminates the whole NRT update for reliability reasons
					return nrt
				}
				updatedNRT.Zones[zoneIdx].Resources[resIdx].Available = tmp
				break
			}
		}
	}
	return updatedNRT
}
