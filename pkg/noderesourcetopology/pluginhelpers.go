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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
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
	nodeTopologyInformer := topologyInformerFactory.Topology().V1alpha2().NodeResourceTopologies()
	nodeTopologyLister := nodeTopologyInformer.Lister()

	klog.V(5).InfoS("Start nodeTopologyInformer")
	ctx := context.Background()
	topologyInformerFactory.Start(ctx.Done())
	topologyInformerFactory.WaitForCacheSync(ctx.Done())

	if tcfg.DiscardReservedNodes {
		return nrtcache.NewDiscardReserved(nodeTopologyLister), nil
	}

	if tcfg.CacheResyncPeriodSeconds <= 0 {
		return nrtcache.NewPassthrough(nodeTopologyLister), nil
	}

	podSharedInformer := nrtcache.InformerFromHandle(handle)
	podLister := handle.SharedInformerFactory().Core().V1().Pods().Lister()

	nrtCache, err := nrtcache.NewOverReserve(nodeTopologyLister, podLister)
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

func createNUMANodeList(zones topologyv1alpha2.ZoneList) NUMANodeList {
	nodes := make(NUMANodeList, 0, len(zones))
	for _, zone := range zones {
		if zone.Type != "Node" {
			continue
		}
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
	return nodes
}

func extractResources(zone topologyv1alpha2.Zone) corev1.ResourceList {
	res := make(corev1.ResourceList)
	for _, resInfo := range zone.Resources {
		res[corev1.ResourceName(resInfo.Name)] = resInfo.Available.DeepCopy()
	}
	return res
}
