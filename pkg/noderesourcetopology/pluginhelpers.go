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
	"encoding/json"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	topoclientset "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
	topologyinformers "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/informers/externalversions"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

func initNodeTopologyInformer(tcfg *apiconfig.NodeResourceTopologyMatchArgs, handle framework.Handle) (nrtcache.Interface, error) {
	topoClient, err := topoclientset.NewForConfig(handle.KubeConfig())
	if err != nil {
		klog.ErrorS(err, "Cannot create clientset for NodeTopologyResource", "kubeConfig", handle.KubeConfig())
		return nil, err
	}

	topologyInformerFactory := topologyinformers.NewSharedInformerFactory(topoClient, 0)
	nodeTopologyInformer := topologyInformerFactory.Topology().V1alpha1().NodeResourceTopologies()
	nodeTopologyLister := nodeTopologyInformer.Lister()

	klog.V(5).InfoS("Start nodeTopologyInformer")
	ctx := context.Background()
	topologyInformerFactory.Start(ctx.Done())
	topologyInformerFactory.WaitForCacheSync(ctx.Done())

	if tcfg.CacheResyncPeriodSeconds <= 0 {
		return nrtcache.NewPassthrough(nodeTopologyLister), nil
	}

	podSharedInformer := nrtcache.InformerFromHandle(handle)
	podIndexer := nrtcache.NewNodeNameIndexer(podSharedInformer)
	nrtCache, err := nrtcache.NewOverReserve(nodeTopologyLister, podIndexer)
	if err != nil {
		return nil, err
	}

	if fwk, ok := handle.(framework.Framework); ok {
		profileName := fwk.ProfileName()
		klog.InfoS("setting up foreign pods detection", "name", profileName)
		nrtcache.RegisterSchedulerProfileName(profileName)
		nrtcache.SetupForeignPodsDetector(profileName, podSharedInformer, nrtCache)
	} else {
		klog.Warningf("cannot determine the scheduler profile names - no foreign pod detection enabled")
	}

	resyncPeriod := time.Duration(tcfg.CacheResyncPeriodSeconds) * time.Second
	go wait.Forever(nrtCache.Resync, resyncPeriod)

	klog.V(3).InfoS("enable NodeTopology cache (needs the Reserve plugin)", "resyncPeriod", resyncPeriod)

	return nrtCache, nil
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
			klog.V(6).InfoS("extracted NUMA resources", stringify.ResourceListToLoggable(zone.Name, resources)...)
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

func makePodByResourceLists(resources ...v1.ResourceList) *v1.Pod {
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{},
		},
	}

	for _, r := range resources {
		pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
			Resources: v1.ResourceRequirements{
				Requests: r,
				Limits:   r,
			},
		})
	}

	return pod
}

func makePodWithReqByResourceList(resources *v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: *resources,
					},
				},
			},
		},
	}
}

func makePodWithReqAndLimitByResourceList(resourcesReq, resourcesLim *v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: *resourcesReq,
						Limits:   *resourcesLim,
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
		res[v1.ResourceName(resInfo.Name)] = resInfo.Available
	}
	return res
}

func newFilterHandlers() filterHandlersMap {
	return filterHandlersMap{
		topologyv1alpha1.SingleNUMANodePodLevel:       singleNUMAPodLevelHandler,
		topologyv1alpha1.SingleNUMANodeContainerLevel: singleNUMAContainerLevelHandler,
	}
}

func newScoringHandlers(strategy scoreStrategy, resourceToWeightMap resourceToWeightMap) scoreHandlersMap {
	return scoreHandlersMap{
		topologyv1alpha1.SingleNUMANodePodLevel: func(pod *v1.Pod, zones topologyv1alpha1.ZoneList) (int64, *framework.Status) {
			return podScopeScore(pod, zones, strategy, resourceToWeightMap)
		},
		topologyv1alpha1.SingleNUMANodeContainerLevel: func(pod *v1.Pod, zones topologyv1alpha1.ZoneList) (int64, *framework.Status) {
			return containerScopeScore(pod, zones, strategy, resourceToWeightMap)
		},
	}
}

func logNumaNodes(desc, nodeName string, nodes NUMANodeList) {
	for _, numaNode := range nodes {
		numaLogKey := fmt.Sprintf("%s/node-%d", nodeName, numaNode.NUMAID)
		klog.V(6).InfoS(desc, stringify.ResourceListToLoggable(numaLogKey, numaNode.Resources)...)
	}
}

func logNRT(desc string, nrtObj *topologyv1alpha1.NodeResourceTopology) {
	if !klog.V(6).Enabled() {
		// avoid the expensive marshal operation
		return
	}

	ntrJson, err := json.MarshalIndent(nrtObj, "", " ")
	if err != nil {
		klog.V(6).ErrorS(err, "failed to marshal noderesourcetopology object")
		return
	}
	klog.V(6).Info(desc, "noderesourcetopology", string(ntrJson))
}
