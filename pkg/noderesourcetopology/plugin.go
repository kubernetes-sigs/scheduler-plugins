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
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/validation"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"

	"github.com/go-logr/logr"
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

type filterInfo struct {
	nodeName        string // shortcut, used very often
	node            fwk.NodeInfo
	topologyManager nodeconfig.TopologyManager
	numaNodes       NUMANodeList
	qos             v1.PodQOSClass
}

type filterFn func(logr.Logger, *v1.Pod, *filterInfo) *fwk.Status

type scoreInfo struct {
	topologyManager nodeconfig.TopologyManager
	qos             v1.PodQOSClass
	numaNodes       NUMANodeList
}

type scoringFn func(logr.Logger, *v1.Pod, *scoreInfo) (int64, *fwk.Status)

// TopologyMatch plugin which run simplified version of TopologyManager's admit handler
type TopologyMatch struct {
	logger              klog.Logger
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
func New(ctx context.Context, args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	lh := klog.FromContext(ctx)

	lh.V(5).Info("creating new noderesourcetopology plugin")
	tcfg, ok := args.(*apiconfig.NodeResourceTopologyMatchArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NodeResourceTopologyMatchArgs, got %T", args)
	}

	if err := validation.ValidateNodeResourceTopologyMatchArgs(nil, tcfg); err != nil {
		return nil, err
	}

	nrtCache, err := initNodeTopologyInformer(ctx, lh, tcfg, handle)
	if err != nil {
		lh.Error(err, "cannot create clientset for NodeTopologyResource", "kubeConfig", handle.KubeConfig())
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
		logger:              lh,
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
func (tm *TopologyMatch) EventsToRegister(_ context.Context) ([]fwk.ClusterEventWithHint, error) {
	// To register a custom event, follow the naming convention at:
	// https://github.com/kubernetes/kubernetes/pull/101394
	// Please follow: eventhandlers.go#L403-L410
	nrtGVK := fmt.Sprintf("noderesourcetopologies.v1alpha2.%v", topologyapi.GroupName)
	return []fwk.ClusterEventWithHint{
		{Event: fwk.ClusterEvent{Resource: fwk.Pod, ActionType: fwk.Delete}},
		{Event: fwk.ClusterEvent{Resource: fwk.Node, ActionType: fwk.Add | fwk.UpdateNodeAllocatable}},
		{Event: fwk.ClusterEvent{Resource: fwk.EventResource(nrtGVK), ActionType: fwk.Add | fwk.Update}},
	}, nil
}
