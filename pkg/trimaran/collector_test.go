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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

var (
	args = pluginConfig.TrimaranSpec{
		WatcherAddress: "http://deadbeef:2020",
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

	noWatcherResponseForNode = watcher.WatcherMetrics{
		Window: watcher.Window{},
		Data: watcher.Data{
			NodeMetricsMap: map[string]watcher.NodeMetrics{},
		},
	}
)

func TestNewCollector(t *testing.T) {
	col, err := NewCollector(&args)
	assert.NotNil(t, col)
	assert.Nil(t, err)
}

func TestNewCollectorSpecs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	metricProvider := pluginConfig.MetricProviderSpec{
		Type: "",
	}
	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: "",
		MetricProvider: metricProvider,
	}

	col, err := NewCollector(&trimaranSpec)
	assert.Nil(t, col)
	expectedErr := "invalid MetricProvider.Type, got " + string(metricProvider.Type)
	assert.EqualError(t, err, expectedErr)
}

func TestGetAllMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: server.URL,
	}
	collector, err := NewCollector(&trimaranSpec)
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

	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: server.URL,
	}
	collector, err := NewCollector(&trimaranSpec)
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

	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: server.URL,
	}
	collector, err := NewCollector(&trimaranSpec)
	assert.NotNil(t, collector)
	assert.Nil(t, err)
	nodeName := "node-1"
	metrics, allMetrics := collector.GetNodeMetrics(nodeName)
	expectedMetrics := watcherResponse.Data.NodeMetricsMap[nodeName].Metrics
	assert.EqualValues(t, expectedMetrics, metrics)
	expectedAllMetrics := &watcherResponse
	assert.EqualValues(t, expectedAllMetrics, allMetrics)
}

func TestGetNodeMetricsNilForNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(noWatcherResponseForNode)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: server.URL,
	}
	collector, err := NewCollector(&trimaranSpec)
	assert.NotNil(t, collector)
	assert.Nil(t, err)
	nodeName := "node-1"
	metrics, allMetrics := collector.GetNodeMetrics(nodeName)
	expectedMetrics := noWatcherResponseForNode.Data.NodeMetricsMap[nodeName].Metrics
	assert.EqualValues(t, expectedMetrics, metrics)
	assert.NotNil(t, allMetrics)
	expectedAllMetrics := &noWatcherResponseForNode
	assert.EqualValues(t, expectedAllMetrics, allMetrics)
}

func TestNewCollectorLoadWatcher(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	metricProvider := pluginConfig.MetricProviderSpec{
		Type:    watcher.SignalFxClientName,
		Address: server.URL,
		Token:   "PWNED",
	}
	trimaranSpec := pluginConfig.TrimaranSpec{
		WatcherAddress: "",
		MetricProvider: metricProvider,
	}

	col, err := NewCollector(&trimaranSpec)
	assert.NotNil(t, col)
	assert.Nil(t, err)
}
