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
	"strconv"
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"
	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	loadWatcherServiceClientName = "load-watcher"
	defaultMetricProviderType    = watcher.K8sClientName
	defaultSafeVarianceMargin    = "1"

	metricsUpdateIntervalSeconds = 30
)

// Collector : get data from load watcher, encapsulating the load watcher and its operations
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

	var metricclient loadwatcherapi.Client
	if args.MetricProviderType == loadWatcherServiceClientName {
		metricclient, _ = loadwatcherapi.NewServiceClient(args.WatcherAddress)
	} else {
		metricproviderops := watcher.MetricsProviderOpts{
			Name:      args.MetricProviderType,
			Address:   args.MetricProviderAddress,
			AuthToken: args.MetricProviderToken,
		}
		metricclient, _ = loadwatcherapi.NewLibraryClient(metricproviderops)
	}

	collector := &Collector{
		client: metricclient,
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

// getSafeVarianceMargin : get the safe variance margin argument
func (collector *Collector) getSafeVarianceMargin() (float64, error) {
	return strconv.ParseFloat(collector.args.SafeVarianceMargin, 64)
}

// getArgs : get configured args
func getArgs(obj runtime.Object) *pluginConfig.LoadVariationRiskBalancingArgs {
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		klog.Errorf("want args to be of type LoadVariationRiskBalancingArgs, got %T, using defaults", obj)
		args = &pluginConfig.LoadVariationRiskBalancingArgs{
			MetricProviderType: defaultMetricProviderType,
			SafeVarianceMargin: defaultSafeVarianceMargin,
		}
		return args
	}
	// check option to use load watcher service
	if args.WatcherAddress != "" {
		args.MetricProviderType = loadWatcherServiceClientName
		args.MetricProviderAddress = args.WatcherAddress
	}
	//check validity of provider type
	if args.MetricProviderType == "" {
		args.MetricProviderType = defaultMetricProviderType
	}
	// check validity of safe variance margin
	defaultMargin, _ := strconv.ParseFloat(defaultSafeVarianceMargin, 64)
	margin, err := strconv.ParseFloat(args.SafeVarianceMargin, 64)
	if err != nil {
		klog.Errorf("unable to parse SafeVarianceMargin %s, using default %s", args.SafeVarianceMargin, defaultSafeVarianceMargin)
		args.SafeVarianceMargin = defaultSafeVarianceMargin
		margin = defaultMargin
	}
	if margin < 0 {
		klog.Errorf("bad value for safe variance margin %f, using default %f", margin, defaultMargin)
		args.SafeVarianceMargin = defaultSafeVarianceMargin
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
