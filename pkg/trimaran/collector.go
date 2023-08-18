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

package trimaran

import (
	"fmt"
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"

	"k8s.io/klog/v2"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

const (
	metricsUpdateIntervalSeconds = 30
)

// Collector : get data from load watcher, encapsulating the load watcher and its operations
//
// Trimaran plugins have different, potentially conflicting, objectives. Thus, it is recommended not
// to enable them concurrently. As such, they are currently designed to each have its own Collector.
// If a need arises in the future to enable multiple Trimaran plugins, a restructuring to have a single
// Collector, serving the multiple plugins, may be beneficial for performance reasons.
type Collector struct {
	// load watcher client
	client loadwatcherapi.Client
	// data collected by load watcher
	metrics watcher.WatcherMetrics
	// for safe access to metrics
	mu sync.RWMutex
}

// NewCollector : create an instance of a data collector
func NewCollector(trimaranSpec *pluginConfig.TrimaranSpec) (*Collector, error) {
	if err := checkSpecs(trimaranSpec); err != nil {
		return nil, err
	}
	klog.V(4).InfoS("Using TrimaranSpec", "type", trimaranSpec.MetricProvider.Type,
		"address", trimaranSpec.MetricProvider.Address, "watcher", trimaranSpec.WatcherAddress)

	var client loadwatcherapi.Client
	if trimaranSpec.WatcherAddress != "" {
		client, _ = loadwatcherapi.NewServiceClient(trimaranSpec.WatcherAddress)
	} else {
		opts := watcher.MetricsProviderOpts{
			Name:               string(trimaranSpec.MetricProvider.Type),
			Address:            trimaranSpec.MetricProvider.Address,
			AuthToken:          trimaranSpec.MetricProvider.Token,
			InsecureSkipVerify: trimaranSpec.MetricProvider.InsecureSkipVerify,
		}
		client, _ = loadwatcherapi.NewLibraryClient(opts)
	}

	collector := &Collector{
		client: client,
	}

	// populate metrics before returning
	err := collector.updateMetrics()
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

// GetNodeMetrics : get metrics for a node from watcher
func (collector *Collector) GetNodeMetrics(nodeName string) ([]watcher.Metric, *watcher.WatcherMetrics) {
	allMetrics := collector.getAllMetrics()
	// This happens if metrics were never populated since scheduler started
	if allMetrics.Data.NodeMetricsMap == nil {
		klog.ErrorS(nil, "Metrics not available from watcher")
		return nil, nil
	}
	// Check if node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := allMetrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.ErrorS(nil, "Unable to find metrics for node", "nodeName", nodeName)
		return nil, allMetrics
	}
	return allMetrics.Data.NodeMetricsMap[nodeName].Metrics, allMetrics
}

// checkSpecs : check trimaran specs
func checkSpecs(trimaranSpec *pluginConfig.TrimaranSpec) error {
	if trimaranSpec.WatcherAddress == "" {
		metricProviderType := string(trimaranSpec.MetricProvider.Type)
		validMetricProviderType := metricProviderType == string(pluginConfig.KubernetesMetricsServer) ||
			metricProviderType == string(pluginConfig.Prometheus) ||
			metricProviderType == string(pluginConfig.SignalFx)
		if !validMetricProviderType {
			return fmt.Errorf("invalid MetricProvider.Type, got %v", trimaranSpec.MetricProvider.Type)
		}
	}
	return nil
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
