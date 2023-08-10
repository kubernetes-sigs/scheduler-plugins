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

// Package loadvariationriskbalancing plugin attempts to balance the risk in load variation
// across the cluster. Risk is expressed as the sum of average and standard deviation of
// the measured load on a node. Typical load balancing involves only the average load.
// Here, we consider the variation in load as well, hence resulting in a safer balance.
package loadvariationriskbalancing

import (
	"context"
	"fmt"
	"math"

	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	// Name : name of plugin
	Name = "LoadVariationRiskBalancing"
)

// LoadVariationRiskBalancing : scheduler plugin
type LoadVariationRiskBalancing struct {
	handle       framework.Handle
	eventHandler *trimaran.PodAssignEventHandler
	collector    *trimaran.Collector
	args         *pluginConfig.LoadVariationRiskBalancingArgs
}

var _ framework.ScorePlugin = &LoadVariationRiskBalancing{}

// New : create an instance of a LoadVariationRiskBalancing plugin
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(4).InfoS("Creating new instance of the LoadVariationRiskBalancing plugin")
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type LoadVariationRiskBalancingArgs, got %T", obj)
	}
	collector, err := trimaran.NewCollector(&args.TrimaranSpec)
	if err != nil {
		return nil, err
	}
	klog.V(4).InfoS("Using LoadVariationRiskBalancingArgs", "margin", args.SafeVarianceMargin, "sensitivity", args.SafeVarianceSensitivity)

	podAssignEventHandler := trimaran.New()
	podAssignEventHandler.AddToHandle(handle)

	pl := &LoadVariationRiskBalancing{
		handle:       handle,
		eventHandler: podAssignEventHandler,
		collector:    collector,
		args:         args,
	}
	return pl, nil
}

// Score : evaluate score for a node
func (pl *LoadVariationRiskBalancing) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	klog.V(6).InfoS("Calculating score", "pod", klog.KObj(pod), "nodeName", nodeName)
	score := framework.MinNodeScore
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	// get node metrics
	metrics, _ := pl.collector.GetNodeMetrics(nodeName)
	if metrics == nil {
		klog.InfoS("Failed to get metrics for node; using minimum score", "nodeName", nodeName)
		return score, nil
	}
	podRequest := trimaran.GetResourceRequested(pod)
	node := nodeInfo.Node()

	// calculate CPU score
	var cpuScore float64 = 0
	cpuStats, cpuOK := trimaran.CreateResourceStats(metrics, node, podRequest, v1.ResourceCPU, watcher.CPU)
	if cpuOK {
		cpuScore = computeScore(cpuStats, pl.args.SafeVarianceMargin, pl.args.SafeVarianceSensitivity)
	}
	klog.V(6).InfoS("Calculating CPUScore", "pod", klog.KObj(pod), "nodeName", nodeName, "cpuScore", cpuScore)
	// calculate Memory score
	var memoryScore float64 = 0
	memoryStats, memoryOK := trimaran.CreateResourceStats(metrics, node, podRequest, v1.ResourceMemory, watcher.Memory)
	if memoryOK {
		memoryScore = computeScore(memoryStats, pl.args.SafeVarianceMargin, pl.args.SafeVarianceSensitivity)
	}
	klog.V(6).InfoS("Calculating MemoryScore", "pod", klog.KObj(pod), "nodeName", nodeName, "memoryScore", memoryScore)
	// calculate total score
	var totalScore float64 = 0
	if memoryOK && cpuOK {
		totalScore = math.Min(memoryScore, cpuScore)
	} else {
		totalScore = math.Max(memoryScore, cpuScore)
	}
	score = int64(math.Round(totalScore))
	klog.V(6).InfoS("Calculating totalScore", "pod", klog.KObj(pod), "nodeName", nodeName, "totalScore", score)
	return score, framework.NewStatus(framework.Success, "")
}

// Name : name of plugin
func (pl *LoadVariationRiskBalancing) Name() string {
	return Name
}

// ScoreExtensions : an interface for Score extended functionality
func (pl *LoadVariationRiskBalancing) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// NormalizeScore : normalize scores
func (pl *LoadVariationRiskBalancing) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}
