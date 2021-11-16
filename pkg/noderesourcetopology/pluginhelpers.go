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
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	topoclientset "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"
	listerv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha1"
)

func findNodeTopology(nodeName string, lister listerv1alpha1.NodeResourceTopologyLister) *topologyv1alpha1.NodeResourceTopology {
	klog.V(5).InfoS("Lister for nodeResTopoPlugin", "lister", lister)
	nodeTopology, err := lister.Get(nodeName)
	if err != nil {
		klog.V(5).ErrorS(err, "Cannot get NodeTopologies from NodeResourceTopologyLister")
		return nil
	}
	return nodeTopology
}

func initNodeTopologyInformer(kubeConfig *restclient.Config) (listerv1alpha1.NodeResourceTopologyLister, error) {
	topoClient, err := topoclientset.NewForConfig(kubeConfig)
	if err != nil {
		klog.ErrorS(err, "Cannot create clientset for NodeTopologyResource", "kubeConfig", kubeConfig)
		return nil, err
	}

	topologyInformerFactory := topologyinformers.NewSharedInformerFactory(topoClient, 0)
	nodeTopologyInformer := topologyInformerFactory.Topology().V1alpha1().NodeResourceTopologies()
	nodeResourceTopologyLister := nodeTopologyInformer.Lister()

	klog.V(5).InfoS("Start nodeTopologyInformer")
	ctx := context.Background()
	topologyInformerFactory.Start(ctx.Done())
	topologyInformerFactory.WaitForCacheSync(ctx.Done())

	return nodeResourceTopologyLister, nil
}

func createNUMANodeList(zones topologyv1alpha1.ZoneList) NUMANodeList {
	nodes := make(NUMANodeList, 0)
	for _, zone := range zones {
		if zone.Type == "Node" {
			var numaID int
			_, err := fmt.Sscanf(zone.Name, "node-%d", &numaID)
			if err != nil {
				klog.ErrorS(nil, "Invalid zone format", "zone", zone.Name)
				continue
			}
			if numaID > 63 || numaID < 0 {
				klog.ErrorS(nil, "Invalid NUMA id range", "numaID", numaID)
				continue
			}
			resources := extractResources(zone)
			nodes = append(nodes, NUMANode{NUMAID: numaID, Resources: resources})
		}
	}
	return nodes
}

func makePodByResourceList(resources *v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: *resources,
						Limits:   *resources,
					},
				},
			},
		},
	}
}

func makeResourceListFromZones(zones topologyv1alpha1.ZoneList) v1.ResourceList {
	result := make(v1.ResourceList)
	for _, zone := range zones {
		for _, resInfo := range zone.Resources {
			resQuantity := resInfo.Available
			if quantity, ok := result[v1.ResourceName(resInfo.Name)]; ok {
				resQuantity.Add(quantity)
			}
			result[v1.ResourceName(resInfo.Name)] = resQuantity
		}
	}
	return result
}

func MakeTopologyResInfo(name, capacity, available string) topologyv1alpha1.ResourceInfo {
	return topologyv1alpha1.ResourceInfo{
		Name:      name,
		Capacity:  resource.MustParse(capacity),
		Available: resource.MustParse(available),
	}
}

func makePodByResourceListWithManyContainers(resources *v1.ResourceList, containerCount int) *v1.Pod {
	var containers []v1.Container

	for i := 0; i < containerCount; i++ {
		containers = append(containers, v1.Container{
			Resources: v1.ResourceRequirements{
				Requests: *resources,
				Limits:   *resources,
			},
		})
	}
	return &v1.Pod{
		Spec: v1.PodSpec{
			Containers: containers,
		},
	}
}

func extractResources(zone topologyv1alpha1.Zone) v1.ResourceList {
	res := make(v1.ResourceList)
	for _, resInfo := range zone.Resources {
		klog.V(5).InfoS("Extract resources for zone", "resName", resInfo.Name, "resAvailable", resInfo.Available)
		res[v1.ResourceName(resInfo.Name)] = resInfo.Available
	}
	return res
}

func newPolicyHandlerMap() PolicyHandlerMap {
	return PolicyHandlerMap{
		topologyv1alpha1.SingleNUMANodePodLevel:       newPodScopedHandler(),
		topologyv1alpha1.SingleNUMANodeContainerLevel: newContainerScopedHandler(),
	}
}
