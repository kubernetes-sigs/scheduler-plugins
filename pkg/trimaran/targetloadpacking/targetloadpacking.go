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
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config/v1beta1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	// Time interval in seconds for each metrics agent ingestion.
	metricsAgentReportingIntervalSeconds = 60
	metricsUpdateIntervalSeconds         = 30
	LoadWatcherServiceClientName         = "load-watcher"
	Name                                 = "TargetLoadPacking"
)

var (
	requestsMilliCores           = v1beta1.DefaultRequestsMilliCores
	hostTargetUtilizationPercent = v1beta1.DefaultTargetUtilizationPercent
	requestsMultiplier           float64
)

type TargetLoadPacking struct {
	handle       framework.FrameworkHandle
	client       loadwatcherapi.Client
	metrics      watcher.WatcherMetrics
	eventHandler *trimaran.PodAssignEventHandler
	// For safe access to metrics
	mu sync.RWMutex
}

func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	hostTargetUtilizationPercent = args.TargetUtilization
	requestsMilliCores = args.DefaultRequests.Cpu().MilliValue()
	requestsMultiplier, _ = strconv.ParseFloat(args.DefaultRequestsMultiplier, 64)

	podAssignEventHandler := trimaran.New()

	var client loadwatcherapi.Client
	if args.WatcherAddress != "" {
		client, err = loadwatcherapi.NewServiceClient(args.WatcherAddress)
	} else {
		opts := watcher.MetricsProviderOpts{string(args.MetricProvider.Type), args.MetricProvider.Address, args.MetricProvider.Token}
		client, err = loadwatcherapi.NewLibraryClient(opts)
	}

	pl := &TargetLoadPacking{
		handle:       handle,
		client:       client,
		eventHandler: podAssignEventHandler,
	}

	pl.handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return isAssigned(t)
				case cache.DeletedFinalStateUnknown:
					if pod, ok := t.Obj.(*v1.Pod); ok {
						return isAssigned(pod)
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, pl))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", pl, obj))
					return false
				}
			},
			Handler: podAssignEventHandler,
		},
	)

	// populate metrics before returning
	err = pl.updateMetrics()
	if err != nil {
		klog.Warningf("unable to populate metrics initially: %v", err)
	}
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			err = pl.updateMetrics()
			if err != nil {
				klog.Warningf("unable to update metrics: %v", err)
			}
		}
	}()

	return pl, nil
}

func (pl *TargetLoadPacking) updateMetrics() error {
	metrics, err := pl.client.GetLatestWatcherMetrics()
	if err != nil {
		return err
	}

	pl.mu.Lock()
	pl.metrics = *metrics
	pl.mu.Unlock()

	return nil
}

func (pl *TargetLoadPacking) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (*pluginConfig.TargetLoadPackingArgs, error) {
	targetLoadPackingArgs, ok := obj.(*pluginConfig.TargetLoadPackingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type TargetLoadPackingArgs, got %T", obj)
	}
	if targetLoadPackingArgs.WatcherAddress == "" {
		if targetLoadPackingArgs.MetricProvider.Type == "" {
			targetLoadPackingArgs.MetricProvider.Type = pluginConfig.KubernetesMetricsServer
		} else {
			if targetLoadPackingArgs.MetricProvider.Type != pluginConfig.KubernetesMetricsServer && targetLoadPackingArgs.MetricProvider.Type != pluginConfig.Prometheus && targetLoadPackingArgs.MetricProvider.Type != pluginConfig.SignalFx {
				return nil, fmt.Errorf("invalid MetricProvider.Type, got %T", targetLoadPackingArgs.MetricProvider.Type)
			}
		}
	}

	_, err := strconv.ParseFloat(targetLoadPackingArgs.DefaultRequestsMultiplier, 64)
	if err != nil {
		return nil, errors.New("unable to parse DefaultRequestsMultiplier: " + err.Error())
	}
	return targetLoadPackingArgs, nil
}

func (pl *TargetLoadPacking) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return framework.MinNodeScore, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// copy value lest updateMetrics() updates it and to avoid locking for rest of the function
	pl.mu.RLock()
	metrics := pl.metrics
	pl.mu.RUnlock()
	// This means the node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := metrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.V(6).Infof("unable to find metrics for node %v", nodeName)
		// Avoid the node by scoring minimum
		return framework.MinNodeScore, nil
		//TODO(aqadeer): If this happens for a long time, fall back to allocation based packing. This could mean maintaining failure state across cycles if scheduler doesn't provide this state
	}

	var curPodCPUUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += PredictUtilisation(&container)
	}
	klog.V(6).Infof("predicted utilization for pod %v: %v", pod.Name, curPodCPUUsage)
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
	}

	var nodeCPUUtilPercent float64
	var cpuMetricFound bool
	for _, metric := range metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			if metric.Operator == watcher.Average || metric.Operator == watcher.Latest {
				nodeCPUUtilPercent = metric.Value
				cpuMetricFound = true
			}
		}
	}

	if !cpuMetricFound {
		klog.Errorf("cpu metric not found for node %v in node metrics %v", nodeName, metrics.Data.NodeMetricsMap[nodeName].Metrics)
		return framework.MinNodeScore, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	klog.V(6).Infof("node %v CPU Utilization (millicores): %v, Capacity: %v", nodeName, nodeCPUUtilMillis, nodeCPUCapMillis)

	var missingCPUUtilMillis int64 = 0
	pl.eventHandler.RLock()
	for _, info := range pl.eventHandler.ScheduledPodsCache[nodeName] {
		// If the time stamp of the scheduled pod is outside fetched metrics window, or it is within metrics reporting interval seconds, we predict util.
		// Note that the second condition doesn't guarantee metrics for that pod are not reported yet as the 0 <= t <= 2*metricsAgentReportingIntervalSeconds
		// t = metricsAgentReportingIntervalSeconds is taken as average case and it doesn't hurt us much if we are
		// counting metrics twice in case actual t is less than metricsAgentReportingIntervalSeconds
		if info.Timestamp.Unix() > metrics.Window.End || info.Timestamp.Unix() <= metrics.Window.End &&
			(metrics.Window.End-info.Timestamp.Unix()) < metricsAgentReportingIntervalSeconds {
			for _, container := range info.Pod.Spec.Containers {
				missingCPUUtilMillis += PredictUtilisation(&container)
			}
			missingCPUUtilMillis += info.Pod.Spec.Overhead.Cpu().MilliValue()
			klog.V(6).Infof("missing utilization for pod %v : %v", info.Pod.Name, missingCPUUtilMillis)
		}
	}
	pl.eventHandler.RUnlock()
	klog.V(6).Infof("missing utilization for node %v : %v", nodeName, missingCPUUtilMillis)

	var predictedCPUUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage) + float64(missingCPUUtilMillis)) / nodeCPUCapMillis
	}
	if predictedCPUUsage > float64(hostTargetUtilizationPercent) {
		if predictedCPUUsage > 100 {
			return framework.MinNodeScore, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := int64(math.Round(50 * (100 - predictedCPUUsage) / (100 - float64(hostTargetUtilizationPercent))))
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	score := int64(math.Round((100-float64(hostTargetUtilizationPercent))*
		predictedCPUUsage/float64(hostTargetUtilizationPercent) + float64(hostTargetUtilizationPercent)))
	klog.V(6).Infof("score for host %v: %v", nodeName, score)
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *TargetLoadPacking) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *TargetLoadPacking) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// Checks and returns true if the pod is assigned to a node
func isAssigned(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
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
