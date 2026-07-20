/*
Copyright 2026 The Kubernetes Authors.

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

// Package highloadfilter filters nodes using measured CPU and memory
// utilization supplied by load-watcher.
package highloadfilter

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	configvalidation "sigs.k8s.io/scheduler-plugins/apis/config/validation"
)

const (
	// Name is the name of the plugin used in scheduler configuration.
	Name = "HighLoadFilter"

	preFilterStateKey fwk.StateKey = "PreFilter" + Name
)

const (
	ErrReasonMetricsUnavailable = "node real-load metrics are unavailable"
	ErrReasonMetricsStale       = "node real-load metrics are stale"
	ErrReasonCPULoadExceeds     = "node predicted CPU utilization exceeds the HighLoadFilter threshold"
	ErrReasonMemoryLoadExceeds  = "node predicted memory utilization exceeds the HighLoadFilter threshold"
)

type preFilterState struct {
	metrics     *watcher.WatcherMetrics
	podRequests corev1.ResourceList
	unavailable string
}

func (s *preFilterState) Clone() fwk.StateData {
	return &preFilterState{
		metrics:     s.metrics,
		podRequests: s.podRequests.DeepCopy(),
		unavailable: s.unavailable,
	}
}

// HighLoadFilter rejects nodes whose predicted utilization exceeds configured
// thresholds. Predicted utilization is measured utilization plus the incoming
// Pod's requests as a percentage of node allocatable capacity.
type HighLoadFilter struct {
	logger klog.Logger
	source metricsSource
	args   *pluginconfig.HighLoadFilterArgs
	now    func() time.Time
}

var (
	_ fwk.PreFilterPlugin = &HighLoadFilter{}
	_ fwk.FilterPlugin    = &HighLoadFilter{}
	_ fwk.SignPlugin      = &HighLoadFilter{}
)

// New creates a HighLoadFilter plugin.
func New(ctx context.Context, obj runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)
	args, ok := obj.(*pluginconfig.HighLoadFilterArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type HighLoadFilterArgs, got %T", obj)
	}
	if err := configvalidation.ValidateHighLoadFilterArgs(args, field.NewPath("args")); err != nil {
		return nil, err
	}

	c, err := newCollector(logger, args)
	if err != nil {
		return nil, err
	}
	c.Start(ctx, logger, time.Duration(args.MetricsUpdateIntervalSeconds)*time.Second)

	logger.V(4).Info("Using HighLoadFilter configuration",
		"watcherAddress", args.WatcherAddress,
		"metricProviderType", args.MetricProvider.Type,
		"cpuThreshold", args.UsageThresholds.CPU,
		"memoryThreshold", args.UsageThresholds.Memory,
		"failOpen", args.FailOpen,
		"metricsUpdateIntervalSeconds", args.MetricsUpdateIntervalSeconds,
		"nodeMetricExpirationSeconds", args.NodeMetricExpirationSeconds)

	return &HighLoadFilter{
		logger: logger,
		source: c,
		args:   args,
		now:    time.Now,
	}, nil
}

// Name returns the plugin name.
func (pl *HighLoadFilter) Name() string {
	return Name
}

// PreFilter captures one immutable metrics snapshot and the incoming Pod's
// requests for the whole scheduling cycle.
func (pl *HighLoadFilter) PreFilter(_ context.Context, state fwk.CycleState, pod *corev1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	snapshot := pl.source.Snapshot()
	preState := &preFilterState{
		metrics:     snapshot,
		podRequests: resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}),
	}

	switch {
	case snapshot == nil || snapshot.Data.NodeMetricsMap == nil:
		preState.unavailable = ErrReasonMetricsUnavailable
	case snapshot.Timestamp <= 0:
		preState.unavailable = ErrReasonMetricsUnavailable
	case pl.now().Sub(time.Unix(snapshot.Timestamp, 0)) > time.Duration(pl.args.NodeMetricExpirationSeconds)*time.Second:
		preState.unavailable = ErrReasonMetricsStale
	}

	state.Write(preFilterStateKey, preState)
	return nil, nil
}

// PreFilterExtensions returns nil because the snapshot and Pod requests do not
// need incremental updates during preemption simulation.
func (pl *HighLoadFilter) PreFilterExtensions() fwk.PreFilterExtensions {
	return nil
}

// Filter rejects a node when predicted CPU or memory utilization is greater
// than its configured threshold.
func (pl *HighLoadFilter) Filter(ctx context.Context, state fwk.CycleState, pod *corev1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	if err := context.Cause(ctx); err != nil {
		return fwk.NewStatus(fwk.UnschedulableAndUnresolvable, err.Error())
	}

	node := nodeInfo.Node()
	if node == nil {
		return fwk.NewStatus(fwk.Error, "node not found")
	}

	stateData, err := state.Read(preFilterStateKey)
	if err != nil {
		return fwk.AsStatus(fmt.Errorf("read %s state: %w; enable HighLoadFilter at both PreFilter and Filter", Name, err))
	}
	preState, ok := stateData.(*preFilterState)
	if !ok {
		return fwk.AsStatus(fmt.Errorf("invalid %s preFilter state %T", Name, stateData))
	}
	if preState.unavailable != "" {
		return pl.metricsUnavailableStatus(preState.unavailable)
	}

	nodeMetrics, ok := preState.metrics.Data.NodeMetricsMap[node.Name]
	if !ok {
		return pl.metricsUnavailableStatus(ErrReasonMetricsUnavailable)
	}

	cpuUsage, cpuFound := resourceUsage(nodeMetrics.Metrics, watcher.CPU)
	if cpuFound {
		predicted := predictedCPUUsage(cpuUsage, preState.podRequests, node)
		if predicted > float64(pl.args.UsageThresholds.CPU) {
			pl.logger.V(4).Info("Rejecting node because predicted CPU utilization exceeds threshold",
				"pod", klog.KObj(pod), "node", klog.KObj(node), "predictedUtilization", predicted,
				"threshold", pl.args.UsageThresholds.CPU)
			return fwk.NewStatus(fwk.Unschedulable, ErrReasonCPULoadExceeds)
		}
	}

	memoryUsage, memoryFound := resourceUsage(nodeMetrics.Metrics, watcher.Memory)
	if memoryFound {
		predicted := predictedMemoryUsage(memoryUsage, preState.podRequests, node)
		if predicted > float64(pl.args.UsageThresholds.Memory) {
			pl.logger.V(4).Info("Rejecting node because predicted memory utilization exceeds threshold",
				"pod", klog.KObj(pod), "node", klog.KObj(node), "predictedUtilization", predicted,
				"threshold", pl.args.UsageThresholds.Memory)
			return fwk.NewStatus(fwk.Unschedulable, ErrReasonMemoryLoadExceeds)
		}
	}

	if !cpuFound || !memoryFound {
		return pl.metricsUnavailableStatus(ErrReasonMetricsUnavailable)
	}
	return nil
}

// SignPod allows scheduler batching only for Pods with identical CPU and
// memory requests, which are the Pod fields that influence this plugin.
func (pl *HighLoadFilter) SignPod(_ context.Context, pod *corev1.Pod) ([]fwk.SignFragment, *fwk.Status) {
	requests := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
	cpu := requests[corev1.ResourceCPU]
	memory := requests[corev1.ResourceMemory]
	return []fwk.SignFragment{{
		Key: Name + "/requests",
		Value: struct {
			MilliCPU    int64 `json:"milliCPU"`
			MemoryBytes int64 `json:"memoryBytes"`
		}{
			MilliCPU:    cpu.MilliValue(),
			MemoryBytes: memory.Value(),
		},
	}}, nil
}

func (pl *HighLoadFilter) metricsUnavailableStatus(reason string) *fwk.Status {
	if pl.args.FailOpen {
		return nil
	}
	return fwk.NewStatus(fwk.Unschedulable, reason)
}

func resourceUsage(metrics []watcher.Metric, resourceType string) (float64, bool) {
	var fallback *float64
	for i := range metrics {
		metric := metrics[i]
		if metric.Type != resourceType || math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) || metric.Value < 0 {
			continue
		}
		if metric.Operator == watcher.Average {
			return metric.Value, true
		}
		if metric.Operator == "" || metric.Operator == watcher.Latest {
			value := metric.Value
			fallback = &value
		}
	}
	if fallback == nil {
		return 0, false
	}
	return *fallback, true
}

func predictedCPUUsage(measured float64, requests corev1.ResourceList, node *corev1.Node) float64 {
	capacity := node.Status.Allocatable.Cpu().MilliValue()
	request := requests.Cpu().MilliValue()
	return predictedUsage(measured, request, capacity)
}

func predictedMemoryUsage(measured float64, requests corev1.ResourceList, node *corev1.Node) float64 {
	capacity := node.Status.Allocatable.Memory().Value()
	request := requests.Memory().Value()
	return predictedUsage(measured, request, capacity)
}

func predictedUsage(measured float64, request, capacity int64) float64 {
	if capacity <= 0 {
		if request > 0 {
			return math.Inf(1)
		}
		return measured
	}
	return measured + float64(request)*100/float64(capacity)
}
