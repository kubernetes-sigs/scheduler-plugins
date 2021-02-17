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
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/francoispqt/gojay"
	"github.com/paypal/load-watcher/pkg/watcher"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	// Time interval in seconds for each metrics agent ingestion.
	metricsAgentReportingIntervalSeconds = 60
	httpClientTimeout                    = 55 * time.Second
	metricsUpdateIntervalSeconds         = 30

	watcherBaseURL = "/watcher"

	defaultWatcherAddress     = "http://127.0.0.1:2020"
	defaultSafeVarianceMargin = "1"
)

// collector : get data from load watcher, encapsulating the load watcher and its operations
type collector struct {
	// client to load watcher
	client http.Client
	// data collected by load watcher
	metrics watcher.WatcherMetrics
	// plugin arguments
	args *pluginConfig.LoadVariationRiskBalancingArgs
	// ror safe access to metrics
	mu sync.RWMutex
}

// newCollector : create an instance of a data collector
func newCollector(obj runtime.Object) (*collector, error) {
	// get the plugin arguments
	args := getArgs(obj)
	collector := &collector{
		client: http.Client{
			Timeout: httpClientTimeout,
		},
		args: args,
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
	return collector, err
}

// getAllMetrics : get all metrics from watcher
func (collector *collector) getAllMetrics() *watcher.WatcherMetrics {
	// copy reference lest updateMetrics() updates the value and to avoid locking for rest of the function
	collector.mu.RLock()
	metrics := collector.metrics
	collector.mu.RUnlock()
	return &metrics
}

// getNodeMetrics : get metrics for a node from watcher
func (collector *collector) getNodeMetrics(nodeName string) []watcher.Metric {
	allMetrics := collector.getAllMetrics()
	// Check if node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := allMetrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.Errorf("unable to find metrics for node %v", nodeName)
		return nil
	}
	return allMetrics.Data.NodeMetricsMap[nodeName].Metrics
}

// getSafeVarianceMargin : get the safe variance margin argument
func (collector *collector) getSafeVarianceMargin() (float64, error) {
	return strconv.ParseFloat(collector.args.SafeVarianceMargin, 64)
}

// getArgs : get configured args
func getArgs(obj runtime.Object) *pluginConfig.LoadVariationRiskBalancingArgs {
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		klog.Errorf("want args to be of type LoadVariationRiskBalancingArgs, got %T, using defaults", obj)
		args = &pluginConfig.LoadVariationRiskBalancingArgs{}
		args.WatcherAddress = defaultWatcherAddress
		args.SafeVarianceMargin = defaultSafeVarianceMargin
		return args
	}
	// check validity of watcher address
	if args.WatcherAddress == "" {
		klog.Errorf("no watcher address configured, using default")
		args.WatcherAddress = defaultWatcherAddress
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
func (collector *collector) updateMetrics() error {
	watcherAddress := collector.args.WatcherAddress
	req, err := http.NewRequest(http.MethodGet, watcherAddress+watcherBaseURL, nil)
	if err != nil {
		klog.Errorf("new watcher request failed: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	//TODO(aqadeer): Add a couple of retries for transient errors
	resp, err := collector.client.Do(req)
	if err != nil {
		klog.Errorf("request to watcher failed: %v", err)
		// Reset the metrics to avoid stale metrics. Probably use a timestamp for better control
		collector.mu.Lock()
		collector.metrics = watcher.WatcherMetrics{}
		collector.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	klog.V(6).Infof("received status code %v from watcher", resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		data := watcher.Data{NodeMetricsMap: make(map[string]watcher.NodeMetrics)}
		var metrics = watcher.WatcherMetrics{Data: data}
		dec := gojay.BorrowDecoder(resp.Body)
		defer dec.Release()
		err = dec.Decode(&metrics)
		if err != nil {
			klog.Errorf("unable to decode watcher metrics: %v", err)
		}
		collector.mu.Lock()
		collector.metrics = metrics
		collector.mu.Unlock()
	} else {
		klog.Errorf("received status code %v from watcher", resp.StatusCode)
	}
	return nil
}
