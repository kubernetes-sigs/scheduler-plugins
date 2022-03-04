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

package loadvariationriskbalancing

import (
	"fmt"
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	metricsUpdateIntervalSeconds = 30
)

// Collector : get data from load watcher, encapsulating the load watcher and its operations
//
// Currently, the Collector owns its own load watcher and is used solely by the LoadVariationRiskBalancing
// plugin. Other Trimaran plugins, such as the TargetLoadPacking, have their own load watchers. The reason
// being that the Trimaran plugins have different, potentially conflicting, objectives. Thus, it is recommended
// not to enable them concurrently. As such, they are currently designed to each have its own load-watcher.
// If a need arises in the future to enable multiple Trimaran plugins, a restructuring to have a single Collector,
// serving the multiple plugins, may be beneficial for performance reasons.
type Collector struct {
	// load watcher client
	client loadwatcherapi.Client
	// data collected by load watcher
	metrics watcher.WatcherMetrics
	// plugin arguments
	args *pluginConfig.LoadVariationRiskBalancingArgs
	// for safe access to metrics
	mu sync.RWMutex
}

// newCollector : create an instance of a data collector
func newCollector(obj runtime.Object) (*Collector, error) {
	// get the plugin arguments
	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	klog.V(4).InfoS("Using LoadVariationRiskBalancingArgs", "type", args.MetricProvider.Type, "address", args.MetricProvider.Address, "margin", args.SafeVarianceMargin, "sensitivity", args.SafeVarianceSensitivity, "watcher", args.WatcherAddress)

	var client loadwatcherapi.Client
	if args.WatcherAddress != "" {
		client, _ = loadwatcherapi.NewServiceClient(args.WatcherAddress)
	} else {
		opts := watcher.MetricsProviderOpts{
			Name:               string(args.MetricProvider.Type),
			Address:            args.MetricProvider.Address,
			AuthToken:          args.MetricProvider.Token,
			InsecureSkipVerify: args.MetricProvider.InsecureSkipVerify,
		}
		client, _ = loadwatcherapi.NewLibraryClient(opts)
	}

	collector := &Collector{
		client: client,
		args:   args,
	}

	// populate metrics before returning
	err = collector.updateMetrics()
	if err != nil {
		klog.ErrorS(err, "Unable to populate metrics initially")
	}
	// start periodic updates
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			err = collector.updateMetrics()
			if err != nil {
				klog.ErrorS(err, "Unable to update metrics")
			}
		}
	}()
	return collector, nil
}

// getAllMetrics : get all metrics from watcher
func (collector *Collector) getAllMetrics() *watcher.WatcherMetrics {
	collector.mu.RLock()
	metrics := collector.metrics
	collector.mu.RUnlock()
	return &metrics
}

// getNodeMetrics : get metrics for a node from watcher
func (collector *Collector) getNodeMetrics(nodeName string) []watcher.Metric {
	allMetrics := collector.getAllMetrics()
	// This happens if metrics were never populated since scheduler started
	if allMetrics.Data.NodeMetricsMap == nil {
		klog.ErrorS(nil, "Metrics not available from watcher")
		return nil
	}
	// Check if node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := allMetrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.ErrorS(nil, "Unable to find metrics for node", "nodeName", nodeName)
		return nil
	}
	return allMetrics.Data.NodeMetricsMap[nodeName].Metrics
}

// getArgs : get configured args
func getArgs(obj runtime.Object) (*pluginConfig.LoadVariationRiskBalancingArgs, error) {
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type LoadVariationRiskBalancingArgs, got %T", obj)
	}
	if args.WatcherAddress == "" {
		metricProviderType := string(args.MetricProvider.Type)
		validMetricProviderType := metricProviderType == string(pluginConfig.KubernetesMetricsServer) ||
			metricProviderType == string(pluginConfig.Prometheus) ||
			metricProviderType == string(pluginConfig.SignalFx)
		if !validMetricProviderType {
			return nil, fmt.Errorf("invalid MetricProvider.Type, got %T", args.MetricProvider.Type)
		}
	}
	return args, nil
}

// updateMetrics : request to load watcher to update all metrics
func (collector *Collector) updateMetrics() error {
	metrics, err := collector.client.GetLatestWatcherMetrics()
	if err != nil {
		klog.ErrorS(err, "Load watcher client failed")
		return err
	}
	collector.mu.Lock()
	collector.metrics = *metrics
	collector.mu.Unlock()
	return nil
}
