/*
Copyright 2020 The Kubernetes Authors.

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

/*
targetloadpacking package provides K8s scheduler plugin for best-fit variant of bin packing based on CPU utilization around a target load
It contains plugin for Score extension point.
*/

package targetloadpacking

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/v1beta2"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	Name = "TargetLoadPacking"
	// Time interval in seconds for each metrics agent ingestion.
	metricsAgentReportingIntervalSeconds = 60
)

var (
	requestsMilliCores           = v1beta2.DefaultRequestsMilliCores
	hostTargetUtilizationPercent = v1beta2.DefaultTargetUtilizationPercent
	requestsMultiplier           float64
)

type TargetLoadPacking struct {
	handle       framework.Handle
	eventHandler *trimaran.PodAssignEventHandler
	collector    *trimaran.Collector
	args         *pluginConfig.TargetLoadPackingArgs
}

var _ framework.ScorePlugin = &TargetLoadPacking{}

func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(4).InfoS("Creating new instance of the TargetLoadPacking plugin")
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.TargetLoadPackingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type TargetLoadPackingArgs, got %T", obj)
	}
	collector, err := trimaran.NewCollector(&args.TrimaranSpec)
	if err != nil {
		return nil, err
	}

	hostTargetUtilizationPercent = args.TargetUtilization
	requestsMilliCores = args.DefaultRequests.Cpu().MilliValue()
	requestsMultiplier, err = strconv.ParseFloat(args.DefaultRequestsMultiplier, 64)
	if err != nil {
		return nil, errors.New("unable to parse DefaultRequestsMultiplier: " + err.Error())
	}

	klog.V(4).InfoS("Using TargetLoadPackingArgs",
		"requestsMilliCores", requestsMilliCores,
		"requestsMultiplier", requestsMultiplier,
		"targetUtilization", hostTargetUtilizationPercent)

	podAssignEventHandler := trimaran.New()
	podAssignEventHandler.AddToHandle(handle)

	pl := &TargetLoadPacking{
		handle:       handle,
		eventHandler: podAssignEventHandler,
		collector:    collector,
		args:         args,
	}
	return pl, nil
}

func (pl *TargetLoadPacking) Name() string {
	return Name
}

func (pl *TargetLoadPacking) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	score := framework.MinNodeScore
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// get node metrics
	metrics, allMetrics := pl.collector.GetNodeMetrics(nodeName)
	if metrics == nil {
		klog.InfoS("Failed to get metrics for node; using minimum score", "nodeName", nodeName)
		// Avoid the node by scoring minimum
		return score, nil
		// TODO(aqadeer): If this happens for a long time, fall back to allocation based packing. This could mean maintaining failure state across cycles if scheduler doesn't provide this state

	}

	var curPodCPUUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += PredictUtilisation(&container)
	}
	klog.V(6).InfoS("Predicted utilization for pod", "podName", pod.Name, "cpuUsage", curPodCPUUsage)
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
	}

	var nodeCPUUtilPercent float64
	var cpuMetricFound bool
	for _, metric := range metrics {
		if metric.Type == watcher.CPU {
			if metric.Operator == watcher.Average || metric.Operator == watcher.Latest {
				nodeCPUUtilPercent = metric.Value
				cpuMetricFound = true
			}
		}
	}

	if !cpuMetricFound {
		klog.ErrorS(nil, "Cpu metric not found in node metrics", "nodeName", nodeName, "nodeMetrics", metrics)
		return score, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	klog.V(6).InfoS("Calculating CPU utilization and capacity", "nodeName", nodeName, "cpuUtilMillis", nodeCPUUtilMillis, "cpuCapMillis", nodeCPUCapMillis)

	var missingCPUUtilMillis int64 = 0
	pl.eventHandler.RLock()
	for _, info := range pl.eventHandler.ScheduledPodsCache[nodeName] {
		// If the time stamp of the scheduled pod is outside fetched metrics window, or it is within metrics reporting interval seconds, we predict util.
		// Note that the second condition doesn't guarantee metrics for that pod are not reported yet as the 0 <= t <= 2*metricsAgentReportingIntervalSeconds
		// t = metricsAgentReportingIntervalSeconds is taken as average case and it doesn't hurt us much if we are
		// counting metrics twice in case actual t is less than metricsAgentReportingIntervalSeconds
		if info.Timestamp.Unix() > allMetrics.Window.End || info.Timestamp.Unix() <= allMetrics.Window.End &&
			(allMetrics.Window.End-info.Timestamp.Unix()) < metricsAgentReportingIntervalSeconds {
			for _, container := range info.Pod.Spec.Containers {
				missingCPUUtilMillis += PredictUtilisation(&container)
			}
			missingCPUUtilMillis += info.Pod.Spec.Overhead.Cpu().MilliValue()
			klog.V(6).InfoS("Missing utilization for pod", "podName", info.Pod.Name, "missingCPUUtilMillis", missingCPUUtilMillis)
		}
	}
	pl.eventHandler.RUnlock()
	klog.V(6).InfoS("Missing utilization for node", "nodeName", nodeName, "missingCPUUtilMillis", missingCPUUtilMillis)

	var predictedCPUUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage) + float64(missingCPUUtilMillis)) / nodeCPUCapMillis
	}
	if predictedCPUUsage > float64(hostTargetUtilizationPercent) {
		if predictedCPUUsage > 100 {
			return score, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := int64(math.Round(float64(hostTargetUtilizationPercent) * (100 - predictedCPUUsage) / (100 - float64(hostTargetUtilizationPercent))))
		klog.V(6).InfoS("Penalised score for host", "nodeName", nodeName, "penalisedScore", penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	score = int64(math.Round((100-float64(hostTargetUtilizationPercent))*
		predictedCPUUsage/float64(hostTargetUtilizationPercent) + float64(hostTargetUtilizationPercent)))
	klog.V(6).InfoS("Score for host", "nodeName", nodeName, "score", score)
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *TargetLoadPacking) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *TargetLoadPacking) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// Predict utilization for a container based on its requests/limits
func PredictUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
		return container.Resources.Limits.Cpu().MilliValue()
	} else if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Cpu().MilliValue()) * requestsMultiplier))
	} else {
		return requestsMilliCores
	}
}
