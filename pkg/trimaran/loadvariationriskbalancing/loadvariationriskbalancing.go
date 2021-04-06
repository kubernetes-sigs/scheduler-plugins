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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	// Name : name of plugin
	Name = "LoadVariationRiskBalancing"
)

// LoadVariationRiskBalancing : scheduler plugin
type LoadVariationRiskBalancing struct {
	handle       framework.FrameworkHandle
	eventHandler *trimaran.PodAssignEventHandler
	collector    *Collector
}

// New : create an instance of a LoadVariationRiskBalancing plugin
func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {

	klog.V(4).Infof("Creating new instance of the LoadVariationRiskBalancing plugin")
	collector, err := newCollector(obj)
	if err != nil {
		return nil, err
	}

	podAssignEventHandler := trimaran.New()
	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return isAssigned(t)
				case cache.DeletedFinalStateUnknown:
					if pod, ok := t.Obj.(*v1.Pod); ok {
						return isAssigned(pod)
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod", obj))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object: %T", obj))
					return false
				}
			},
			Handler: podAssignEventHandler,
		},
	)

	pl := &LoadVariationRiskBalancing{
		handle:       handle,
		eventHandler: podAssignEventHandler,
		collector:    collector,
	}
	return pl, nil
}

// Score : evaluate score for a node
func (pl *LoadVariationRiskBalancing) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {

	klog.V(6).Infof("Calculating score for pod %q on node %q", pod.GetName(), nodeName)
	score := framework.MinNodeScore
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	// get node metrics
	metrics := pl.collector.getNodeMetrics(nodeName)
	if metrics == nil {
		klog.Warningf("failure getting metrics for node %q; using minimum score", nodeName)
		return score, nil
	}
	podRequest := getResourceRequested(pod)
	node := nodeInfo.Node()
	// calculate CPU score
	var cpuScore float64 = 0
	cpuStats, cpuOK := createResourceStats(metrics, node, podRequest, v1.ResourceCPU, watcher.CPU)
	if cpuOK {
		cpuScore = cpuStats.computeScore(pl.collector.args.SafeVarianceMargin, pl.collector.args.SafeVarianceSensitivity)
	}
	klog.V(6).Infof("pod:%s; node:%s; CPUScore=%f", pod.GetName(), nodeName, cpuScore)
	// calculate Memory score
	var memoryScore float64 = 0
	memoryStats, memoryOK := createResourceStats(metrics, node, podRequest, v1.ResourceMemory, watcher.Memory)
	if memoryOK {
		memoryScore = memoryStats.computeScore(pl.collector.args.SafeVarianceMargin, pl.collector.args.SafeVarianceSensitivity)
	}
	klog.V(6).Infof("pod:%s; node:%s; MemoryScore=%f", pod.GetName(), nodeName, memoryScore)
	// calculate total score
	var totalScore float64 = 0
	if memoryOK && cpuOK {
		totalScore = math.Min(memoryScore, cpuScore)
	} else {
		totalScore = math.Max(memoryScore, cpuScore)
	}
	score = int64(math.Round(totalScore))
	klog.V(6).Infof("pod:%s; node:%s; TotalScore=%d", pod.GetName(), nodeName, score)
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

// Checks and returns true if the pod is assigned to a node
func isAssigned(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}
