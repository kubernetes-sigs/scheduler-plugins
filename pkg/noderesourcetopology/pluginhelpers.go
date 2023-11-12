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
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

const (
	maxNUMAId = 64
)

func initNodeTopologyInformer(tcfg *apiconfig.NodeResourceTopologyMatchArgs, handle framework.Handle) (nrtcache.Interface, error) {
	client, err := ctrlclient.New(handle.KubeConfig(), ctrlclient.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Cannot create client for NodeTopologyResource", "kubeConfig", handle.KubeConfig())
		return nil, err
	}

	if tcfg.DiscardReservedNodes {
		return nrtcache.NewDiscardReserved(client), nil
	}

	if tcfg.CacheResyncPeriodSeconds <= 0 {
		return nrtcache.NewPassthrough(client), nil
	}

	podSharedInformer, podLister := nrtcache.InformerFromHandle(handle)

	nrtCache, err := nrtcache.NewOverReserve(tcfg.Cache, client, podLister)
	if err != nil {
		return nil, err
	}

	initNodeTopologyForeignPodsDetection(tcfg.Cache, handle, podSharedInformer, nrtCache)

	resyncPeriod := time.Duration(tcfg.CacheResyncPeriodSeconds) * time.Second
	go wait.Forever(nrtCache.Resync, resyncPeriod)

	klog.V(3).InfoS("enable NodeTopology cache (needs the Reserve plugin)", "resyncPeriod", resyncPeriod)

	return nrtCache, nil
}

func initNodeTopologyForeignPodsDetection(cfg *apiconfig.NodeResourceTopologyCache, handle framework.Handle, podSharedInformer k8scache.SharedInformer, nrtCache *nrtcache.OverReserve) {
	foreignPodsDetect := getForeignPodsDetectMode(cfg)

	if foreignPodsDetect == apiconfig.ForeignPodsDetectNone {
		klog.InfoS("foreign pods detection disabled by configuration")
		return
	}
	fwk, ok := handle.(framework.Framework)
	if !ok {
		klog.Warningf("cannot determine the scheduler profile names - no foreign pod detection enabled")
		return
	}

	profileName := fwk.ProfileName()
	klog.InfoS("setting up foreign pods detection", "name", profileName, "mode", foreignPodsDetect)

	if foreignPodsDetect == apiconfig.ForeignPodsDetectOnlyExclusiveResources {
		nrtcache.TrackOnlyForeignPodsWithExclusiveResources()
	} else {
		nrtcache.TrackAllForeignPods()
	}
	nrtcache.RegisterSchedulerProfileName(profileName)
	nrtcache.SetupForeignPodsDetector(profileName, podSharedInformer, nrtCache)
}

func createNUMANodeList(zones topologyv1alpha2.ZoneList) NUMANodeList {
	numaIDToZoneIDx := make([]int, maxNUMAId)
	nodes := NUMANodeList{}
	// filter non Node zones and create idToIdx lookup array
	for i, zone := range zones {
		if zone.Type != "Node" {
			continue
		}

		numaID, err := getID(zone.Name)
		if err != nil {
			klog.Error(err)
			continue
		}

		numaIDToZoneIDx[numaID] = i

		resources := extractResources(zone)
		klog.V(6).InfoS("extracted NUMA resources", stringify.ResourceListToLoggable(zone.Name, resources)...)
		nodes = append(nodes, NUMANode{NUMAID: numaID, Resources: resources})
	}

	// iterate over nodes and fill them with Costs
	for i, node := range nodes {
		nodes[i] = *node.WithCosts(extractCosts(zones[numaIDToZoneIDx[node.NUMAID]].Costs))
	}

	return nodes
}

func getID(name string) (int, error) {
	splitted := strings.Split(name, "-")
	if len(splitted) != 2 {
		return -1, fmt.Errorf("invalid zone format zone: %s", name)
	}

	if splitted[0] != "node" {
		return -1, fmt.Errorf("invalid zone format zone: %s", name)
	}

	numaID, err := strconv.Atoi(splitted[1])
	if err != nil {
		return -1, fmt.Errorf("invalid zone format zone: %s : %v", name, err)
	}

	if numaID > maxNUMAId-1 || numaID < 0 {
		return -1, fmt.Errorf("invalid NUMA id range numaID: %d", numaID)
	}

	return numaID, nil
}

func extractCosts(costs topologyv1alpha2.CostList) map[int]int {
	nodeCosts := make(map[int]int)

	// return early if CostList is missing
	if len(costs) == 0 {
		return nodeCosts
	}

	for _, cost := range costs {
		numaID, err := getID(cost.Name)
		if err != nil {
			continue
		}
		nodeCosts[numaID] = int(cost.Value)
	}

	return nodeCosts
}

func extractResources(zone topologyv1alpha2.Zone) corev1.ResourceList {
	res := make(corev1.ResourceList)
	for _, resInfo := range zone.Resources {
		res[corev1.ResourceName(resInfo.Name)] = resInfo.Available.DeepCopy()
	}
	return res
}

func onlyNonNUMAResources(numaNodes NUMANodeList, resources corev1.ResourceList) bool {
	for resourceName := range resources {
		for _, node := range numaNodes {
			if _, ok := node.Resources[resourceName]; ok {
				return false
			}
		}
	}

	return true
}

func getForeignPodsDetectMode(cfg *apiconfig.NodeResourceTopologyCache) apiconfig.ForeignPodsDetectMode {
	var foreignPodsDetect apiconfig.ForeignPodsDetectMode
	if cfg != nil && cfg.ForeignPodsDetect != nil {
		foreignPodsDetect = *cfg.ForeignPodsDetect
	} else { // explicitly set to nil?
		foreignPodsDetect = apiconfig.ForeignPodsDetectAll
		klog.InfoS("foreign pods detection value missing", "fallback", foreignPodsDetect)
	}
	return foreignPodsDetect
}
