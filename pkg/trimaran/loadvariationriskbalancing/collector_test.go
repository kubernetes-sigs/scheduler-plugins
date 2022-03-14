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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/v1beta2"
)

var (
	args = pluginConfig.LoadVariationRiskBalancingArgs{
		WatcherAddress:     "http://deadbeef:2020",
		SafeVarianceMargin: 1,
	}

	watcherResponse = watcher.WatcherMetrics{
		Window: watcher.Window{},
		Data: watcher.Data{
			NodeMetricsMap: map[string]watcher.NodeMetrics{
				"node-1": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Operator: watcher.Average,
							Value:    80,
						},
						{
							Type:     watcher.CPU,
							Operator: watcher.Std,
							Value:    16,
						},
						{
							Type:     watcher.Memory,
							Operator: watcher.Average,
							Value:    25,
						},
						{
							Type:     watcher.Memory,
							Operator: watcher.Std,
							Value:    6.25,
						},
					},
				},
			},
		},
	}
)

func TestNewCollector(t *testing.T) {
	col, err := newCollector(&args)
	assert.NotNil(t, col)
	assert.Nil(t, err)
}

func TestGetAllMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	loadVariationRiskBalancingArgs := pluginConfig.LoadVariationRiskBalancingArgs{
		WatcherAddress:     server.URL,
		SafeVarianceMargin: v1beta2.DefaultSafeVarianceMargin,
	}
	collector, err := newCollector(&loadVariationRiskBalancingArgs)
	assert.NotNil(t, collector)
	assert.Nil(t, err)

	metrics := collector.getAllMetrics()
	metricsMap := metrics.Data.NodeMetricsMap
	expectedMap := watcherResponse.Data.NodeMetricsMap
	assert.EqualValues(t, expectedMap, metricsMap)
}

func TestUpdateMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	loadVariationRiskBalancingArgs := pluginConfig.LoadVariationRiskBalancingArgs{
		WatcherAddress:     server.URL,
		SafeVarianceMargin: v1beta2.DefaultSafeVarianceMargin,
	}
	collector, err := newCollector(&loadVariationRiskBalancingArgs)
	assert.NotNil(t, collector)
	assert.Nil(t, err)

	err = collector.updateMetrics()
	assert.Nil(t, err)
}

func TestGetNodeMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	loadVariationRiskBalancingArgs := pluginConfig.LoadVariationRiskBalancingArgs{
		WatcherAddress:     server.URL,
		SafeVarianceMargin: v1beta2.DefaultSafeVarianceMargin,
	}
	collector, err := newCollector(&loadVariationRiskBalancingArgs)
	assert.NotNil(t, collector)
	assert.Nil(t, err)
	nodeName := "node-1"
	metrics := collector.getNodeMetrics(nodeName)
	expectedMetrics := watcherResponse.Data.NodeMetricsMap[nodeName].Metrics
	assert.EqualValues(t, expectedMetrics, metrics)
}
