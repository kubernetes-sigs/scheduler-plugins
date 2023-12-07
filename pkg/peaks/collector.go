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

package peaks

import (
	"fmt"
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"
)

const (
	metricsUpdateIntervalSeconds = 30
)

type Collector struct {
	client loadwatcherapi.Client
	metrics watcher.WatcherMetrics
	mu sync.RWMutex
}

func NewCollector(peaksSpec *string) (*Collector, error) {
	var client loadwatcherapi.Client
	client, _ = loadwatcherapi.NewServiceClient(*peaksSpec)

	collector := &Collector{
		client: client,
	}

	err := collector.updateMetrics()
	if err != nil {
		fmt.Println("Unable to get metrics from loadwatcher", err)
	}
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			err = collector.updateMetrics()
			if err != nil {
				fmt.Println(err, "Unable to update metrics")
			}
		}
	}()
	return collector, nil
}

func (c *Collector) getAllMetrics() *watcher.WatcherMetrics {
	c.mu.RLock()
	metrics := c.metrics
	c.mu.RUnlock()
	return &metrics
}

func (collector *Collector) GetAllNodeCpuMetrics() map[string]float64 {
	r := make(map[string]float64)

	allMetrics := collector.getAllMetrics()
	if allMetrics.Data.NodeMetricsMap == nil {
		fmt.Println("Metrics not available from watcher")
		return r
	}

	for node := range allMetrics.Data.NodeMetricsMap {
		for _, metricsele := range allMetrics.Data.NodeMetricsMap[node].Metrics {
			if metricsele.Type == "CPU" {
				r[node] = metricsele.Value
			}
		}
	}
	return r
}

func (collector *Collector) GetNodeMetrics(nodeName string) ([]watcher.Metric, *watcher.WatcherMetrics) {
	allMetrics := collector.getAllMetrics()
	if allMetrics.Data.NodeMetricsMap == nil {
		fmt.Println( "Metrics not available from watcher")
		return nil, nil
	}
	if _, ok := allMetrics.Data.NodeMetricsMap[nodeName]; !ok {
		fmt.Println( "Metrics not available from watcher")
		return nil, allMetrics
	}
	return allMetrics.Data.NodeMetricsMap[nodeName].Metrics, allMetrics
}

func (c *Collector) updateMetrics() error {
	metrics, err := c.client.GetLatestWatcherMetrics()
	fmt.Println(metrics)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.metrics = *metrics
	c.mu.Unlock()
	return nil
}