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
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config/v1beta1"
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
	args := getArgs(obj)

	klog.V(4).Infof("Using LoadVariationRiskBalancingArgs: MetricProvider.Type=%q, MetricProvider.Address=%q,"+
		" SafeVarianceMargin=%f, SafeVarianceSensitivity=%f, WatcherAddress=%q",
		args.MetricProvider.Type, args.MetricProvider.Address, args.SafeVarianceMargin,
		args.SafeVarianceSensitivity, args.WatcherAddress)

	var client loadwatcherapi.Client
	if args.WatcherAddress != "" {
		client, _ = loadwatcherapi.NewServiceClient(args.WatcherAddress)
	} else {
		opts := watcher.MetricsProviderOpts{
			Name:      string(args.MetricProvider.Type),
			Address:   args.MetricProvider.Address,
			AuthToken: args.MetricProvider.Token,
		}
		client, _ = loadwatcherapi.NewLibraryClient(opts)
	}

	collector := &Collector{
		client: client,
		args:   args,
	}

	// populate metrics before returning
	err := collector.updateMetrics()
	if err != nil {
		klog.Warningf("unable to populate metrics initially: %v", err)
	}
	// start periodic updates
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			err = collector.updateMetrics()
			if err != nil {
				klog.Warningf("unable to update metrics: %v", err)
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
	// Check if node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := allMetrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.Errorf("unable to find metrics for node %v", nodeName)
		return nil
	}
	return allMetrics.Data.NodeMetricsMap[nodeName].Metrics
}

// getArgs : get configured args
func getArgs(obj runtime.Object) *pluginConfig.LoadVariationRiskBalancingArgs {
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		klog.Errorf("want args to be of type LoadVariationRiskBalancingArgs, got %T, using defaults", obj)
		args = &pluginConfig.LoadVariationRiskBalancingArgs{
			MetricProvider: pluginConfig.MetricProviderSpec{
				Type: v1beta1.DefaultMetricProviderType,
			},
			SafeVarianceMargin:      v1beta1.DefaultSafeVarianceMargin,
			SafeVarianceSensitivity: v1beta1.DefaultSafeVarianceSensitivity,
		}
		return args
	}
	return args
}

// updateMetrics : request to load watcher to update all metrics
func (collector *Collector) updateMetrics() error {
	metrics, err := collector.client.GetLatestWatcherMetrics()
	if err != nil {
		klog.Errorf("load watcher client failed: %v", err)
		return err
	}
	collector.mu.Lock()
	collector.metrics = *metrics
	collector.mu.Unlock()
	return nil
}
