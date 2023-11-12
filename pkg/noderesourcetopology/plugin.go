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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/validation"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"

	topologyapi "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = "NodeResourceTopologyMatch"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(topologyv1alpha2.AddToScheme(scheme))
}

type NUMANode struct {
	NUMAID    int
	Resources v1.ResourceList
	Costs     map[int]int
}

func (n *NUMANode) WithCosts(costs map[int]int) *NUMANode {
	n.Costs = costs
	return n
}

type NUMANodeList []NUMANode

func subtractFromNUMAs(resources v1.ResourceList, numaNodes NUMANodeList, nodes ...int) {
	for resName, quantity := range resources {
		for _, node := range nodes {
			// quantity is zero no need to iterate through another NUMA node, go to another resource
			if quantity.IsZero() {
				break
			}

			nRes := numaNodes[node].Resources
			if available, ok := nRes[resName]; ok {
				switch quantity.Cmp(available) {
				case 0: // the same
					// basically zero container resources
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case 1: // container wants more resources than available in this NUMA zone
					// substract NUMA resources from container request, to calculate how much is missing
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case -1: // there are more resources available in this NUMA zone than container requests
					// substract container resources from resources available in this NUMA node
					available.Sub(quantity)
					// zero container quantity
					quantity = resource.Quantity{}
					nRes[resName] = available
				}
			}
		}
	}
}

type filterFn func(pod *v1.Pod, zones topologyv1alpha2.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status
type scoringFn func(*v1.Pod, topologyv1alpha2.ZoneList) (int64, *framework.Status)

// TopologyMatch plugin which run simplified version of TopologyManager's admit handler
type TopologyMatch struct {
	resourceToWeightMap resourceToWeightMap
	nrtCache            nrtcache.Interface
	scoreStrategyFunc   scoreStrategyFn
	scoreStrategyType   apiconfig.ScoringStrategyType
}

var _ framework.FilterPlugin = &TopologyMatch{}
var _ framework.ReservePlugin = &TopologyMatch{}
var _ framework.ScorePlugin = &TopologyMatch{}
var _ framework.EnqueueExtensions = &TopologyMatch{}
var _ framework.PostBindPlugin = &TopologyMatch{}

// Name returns name of the plugin. It is used in logs, etc.
func (tm *TopologyMatch) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(5).InfoS("Creating new TopologyMatch plugin")
	tcfg, ok := args.(*apiconfig.NodeResourceTopologyMatchArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NodeResourceTopologyMatchArgs, got %T", args)
	}

	if err := validation.ValidateNodeResourceTopologyMatchArgs(nil, tcfg); err != nil {
		return nil, err
	}

	nrtCache, err := initNodeTopologyInformer(tcfg, handle)
	if err != nil {
		klog.ErrorS(err, "Cannot create clientset for NodeTopologyResource", "kubeConfig", handle.KubeConfig())
		return nil, err
	}

	resToWeightMap := make(resourceToWeightMap)
	for _, resource := range tcfg.ScoringStrategy.Resources {
		resToWeightMap[v1.ResourceName(resource.Name)] = resource.Weight
	}

	// This is not strictly needed, but we do it here and we carry `scoreStrategyFunc` around
	// to be able to do as much parameter validation as possible here in this function.
	// We perform only the NRT-object-specific validation in `Filter()` and `Score()`
	// because we can't help it, being the earliest point in time on which we have access
	// to NRT instances.
	strategy, err := getScoringStrategyFunction(tcfg.ScoringStrategy.Type)
	if err != nil {
		return nil, err
	}

	topologyMatch := &TopologyMatch{
		resourceToWeightMap: resToWeightMap,
		nrtCache:            nrtCache,
		scoreStrategyFunc:   strategy,
		scoreStrategyType:   tcfg.ScoringStrategy.Type,
	}

	return topologyMatch, nil
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
// NOTE: if in-place-update (KEP 1287) gets implemented, then PodUpdate event
// should be registered for this plugin since a Pod update may free up resources
// that make other Pods schedulable.
func (tm *TopologyMatch) EventsToRegister() []framework.ClusterEvent {
	// To register a custom event, follow the naming convention at:
	// https://git.k8s.io/kubernetes/pkg/scheduler/eventhandlers.go#L403-L410
	nrtGVK := fmt.Sprintf("noderesourcetopologies.v1alpha2.%v", topologyapi.GroupName)
	return []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.Delete},
		{Resource: framework.Node, ActionType: framework.Add | framework.UpdateNodeAllocatable},
		{Resource: framework.GVK(nrtGVK), ActionType: framework.Add | framework.Update},
	}
}
