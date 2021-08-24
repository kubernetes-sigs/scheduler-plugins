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

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	topoclientset "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"
	listerv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha1"
)

func findNodeTopology(nodeName string, nodeResTopoPlugin *nodeResTopologyPlugin) *topologyv1alpha1.NodeResourceTopology {
	klog.V(5).Infof("namespaces: %s", nodeResTopoPlugin.namespaces)
	for _, namespace := range nodeResTopoPlugin.namespaces {
		klog.V(5).Infof("data.lister: %v", nodeResTopoPlugin.lister)
		// NodeTopology couldn't be placed in several namespaces simultaneously
		lister := nodeResTopoPlugin.lister
		nodeTopology, err := (*lister).NodeResourceTopologies(namespace).Get(nodeName)
		if err != nil {
			klog.V(5).Infof("Cannot get NodeTopologies from NodeResourceTopologyNamespaceLister: %v", err)
			continue
		}
		if nodeTopology != nil {
			return nodeTopology
		}
	}
	return nil
}

func initNodeTopologyInformer(masterOverride, kubeConfigPath *string) (*listerv1alpha1.NodeResourceTopologyLister, error) {
	kubeConfig, err := clientcmd.BuildConfigFromFlags(*masterOverride, *kubeConfigPath)
	if err != nil {
		klog.Errorf("Cannot create kubeconfig based on: %s, %s, %v", *masterOverride, *kubeConfigPath, err)
		return nil, err
	}

	topoClient, err := topoclientset.NewForConfig(kubeConfig)
	if err != nil {
		klog.Errorf("Cannot create clientset for NodeTopologyResource: %s, %s", kubeConfig, err)
		return nil, err
	}

	topologyInformerFactory := topologyinformers.NewSharedInformerFactory(topoClient, 0)
	nodeTopologyInformer := topologyInformerFactory.Topology().V1alpha1().NodeResourceTopologies()
	nodeResourceTopologyLister := nodeTopologyInformer.Lister()

	klog.V(5).Infof("start nodeTopologyInformer")
	ctx := context.Background()
	topologyInformerFactory.Start(ctx.Done())
	topologyInformerFactory.WaitForCacheSync(ctx.Done())

	return &nodeResourceTopologyLister, nil
}

func createNUMANodeList(zones topologyv1alpha1.ZoneList) NUMANodeList {
	nodes := make(NUMANodeList, 0)
	for _, zone := range zones {
		if zone.Type == "Node" {
			var numaID int
			_, err := fmt.Sscanf(zone.Name, "node-%d", &numaID)
			if err != nil {
				klog.Errorf("Invalid format: %v", zone.Name)
				continue
			}
			if numaID > 63 || numaID < 0 {
				klog.Errorf("Invalid NUMA id range: %v", numaID)
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
			resQuantity, err := resource.ParseQuantity(resInfo.Allocatable.String())
			if err != nil {
				klog.Errorf("Failed to parse %s", resInfo.Allocatable.String())
				continue
			}
			if quantity, ok := result[v1.ResourceName(resInfo.Name)]; ok {
				resQuantity.Add(quantity)
			}
			result[v1.ResourceName(resInfo.Name)] = resQuantity
		}
	}
	return result
}

func makeTopologyResInfo(name, capacity, allocatable string) topologyv1alpha1.ResourceInfo {
	return topologyv1alpha1.ResourceInfo{
		Name:        name,
		Capacity:    intstr.Parse(capacity),
		Allocatable: intstr.Parse(allocatable),
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
		quantity, err := resource.ParseQuantity(resInfo.Allocatable.String())
		klog.V(5).Infof("extractResources: resInfo.FilterPluginName %v, resInfo quantity %d", resInfo.Name, quantity.AsDec())
		if err != nil {
			klog.Errorf("Failed to parse %s", resInfo.Allocatable.String())
			continue
		}
		res[v1.ResourceName(resInfo.Name)] = quantity
	}
	return res
}

func newPolicyHandlerMap() PolicyHandlerMap {
	return PolicyHandlerMap{
		topologyv1alpha1.SingleNUMANodePodLevel:       newPodScopedHandler(),
		topologyv1alpha1.SingleNUMANodeContainerLevel: newContainerScopedHandler(),
	}
}
