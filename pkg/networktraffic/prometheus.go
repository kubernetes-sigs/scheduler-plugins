package networktraffic

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/klog/v2"
)

const (
	// nodeMeasureQueryTemplate is the template string to get the query for the node used bandwidth
	nodeMeasureQueryTemplate = "sum_over_time(node_network_receive_bytes_total{kubernetes_node=\"%s\",device=\"%s\"}[%dm])"
)

// Handles the interaction of the networkplugin with Prometheus
type PrometheusHandle struct {
	networkInterface string
	timeRange        time.Duration
	address          string
	api              v1.API
}

func NewPrometheus(address, networkInterface string, timeRange time.Duration) *PrometheusHandle {
	client, err := api.NewClient(api.Config{
		Address: address,
	})
	if err != nil {
		klog.Fatalf("[NetworkTraffic] Error creating prometheus client: %s", err.Error())
	}

	return &PrometheusHandle{
		networkInterface: networkInterface,
		timeRange:        timeRange,
		address:          address,
		api:              v1.NewAPI(client),
	}
}

func (p *PrometheusHandle) GetNodeBandwidthMeasure(node string) (*model.Sample, error) {
	query := getNodeBandwidthQuery(node, p.networkInterface, p.timeRange)
	res, err := p.query(query)
	if err != nil {
		return nil, fmt.Errorf("[NetworkTraffic] Error querying prometheus: %w", err)
	}

	nodeMeasure := res.(model.Vector)
	if len(nodeMeasure) != 1 {
		return nil, fmt.Errorf("[NetworkTraffic] Invalid response, expected 1 value, got %d", len(nodeMeasure))
	}

	return nodeMeasure[0], nil
}

func getNodeBandwidthQuery(node, networkInterface string, timeRange time.Duration) string {
	return fmt.Sprintf(nodeMeasureQueryTemplate, node, networkInterface, int64(timeRange.Minutes()))
}

func (p *PrometheusHandle) query(query string) (model.Value, error) {
	klog.Infof("[NetworkTraffic] query: %v\n", query)
	results, warnings, err := p.api.Query(context.Background(), query, time.Now())

	if len(warnings) > 0 {
		klog.Warningf("[NetworkTraffic] Warnings: %v\n", warnings)
	}

	klog.Infof("[NetworkTraffic] results: %v\n", results)
	klog.Infof("[NetworkTraffic] err: %v\n", err)
	return results, err
}
