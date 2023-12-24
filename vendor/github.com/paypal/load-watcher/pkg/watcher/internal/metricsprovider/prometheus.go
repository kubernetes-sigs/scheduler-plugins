/*
Copyright 2020

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

package metricsprovider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"k8s.io/client-go/transport"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	log "github.com/sirupsen/logrus"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

const (
	EnableOpenShiftAuth     = "ENABLE_OPENSHIFT_AUTH"
	K8sPodCAFilePath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	DefaultPromAddress      = "http://prometheus-k8s:9090"
	promStd                 = "stddev_over_time"
	promAvg                 = "avg_over_time"
	promCpuMetric           = "instance:node_cpu:ratio"
	promMemMetric           = "instance:node_memory_utilisation:ratio"
	promTransBandMetric     = "instance:node_network_transmit_bytes:rate:sum"
	promTransBandDropMetric = "instance:node_network_transmit_drop_excluding_lo:rate5m"
	promRecBandMetric       = "instance:node_network_receive_bytes:rate:sum"
	promRecBandDropMetric   = "instance:node_network_receive_drop_excluding_lo:rate5m"
	promDiskIOMetric        = "instance_device:node_disk_io_time_seconds:rate5m"
	allHosts                = "all"
	hostMetricKey           = "instance"
)

type promClient struct {
	client api.Client
}

func loadCAFile(filepath string) (*x509.CertPool, error) {
	caCert, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}

	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("failed to append CA certificate to the pool")
	}

	return caCertPool, nil
}

func NewPromClient(opts watcher.MetricsProviderOpts) (watcher.MetricsProviderClient, error) {
	if opts.Name != watcher.PromClientName {
		return nil, fmt.Errorf("metric provider name should be %v, found %v", watcher.PromClientName, opts.Name)
	}

	var client api.Client
	var err error
	var promToken, promAddress = "", DefaultPromAddress
	if opts.AuthToken != "" {
		promToken = opts.AuthToken
	}
	if opts.Address != "" {
		promAddress = opts.Address
	}

	// Ignore TLS verify errors if InsecureSkipVerify is set
	roundTripper := api.DefaultRoundTripper

	// Check if EnableOpenShiftAuth is set.
	_, enableOpenShiftAuth := os.LookupEnv(EnableOpenShiftAuth)
	if enableOpenShiftAuth {
		// Retrieve Pod CA cert
		caCertPool, err := loadCAFile(K8sPodCAFilePath)
		if err != nil {
			return nil, fmt.Errorf("Error loading CA file: %v", err)
		}

		// Get Prometheus Host
		u, _ := url.Parse(opts.Address)
		roundTripper = transport.NewBearerAuthRoundTripper(
			opts.AuthToken,
			&http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				TLSClientConfig: &tls.Config{
					RootCAs:    caCertPool,
					ServerName: u.Host,
				},
			},
		)
	} else if opts.InsecureSkipVerify {
		roundTripper = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		}
	}

	if promToken != "" {
		client, err = api.NewClient(api.Config{
			Address:      promAddress,
			RoundTripper: config.NewAuthorizationCredentialsRoundTripper("Bearer", config.Secret(opts.AuthToken), roundTripper),
		})
	} else {
		client, err = api.NewClient(api.Config{
			Address: promAddress,
		})
	}

	if err != nil {
		log.Errorf("error creating prometheus client: %v", err)
		return nil, err
	}

	return promClient{client}, err
}

func (s promClient) Name() string {
	return watcher.PromClientName
}

func (s promClient) FetchHostMetrics(host string, window *watcher.Window) ([]watcher.Metric, error) {
	var metricList []watcher.Metric
	var anyerr error

	for _, method := range []string{promAvg, promStd} {
		for _, metric := range []string{promCpuMetric, promMemMetric, promTransBandMetric, promTransBandDropMetric, promRecBandMetric, promRecBandDropMetric, promDiskIOMetric} {
			promQuery := s.buildPromQuery(host, metric, method, window.Duration)
			promResults, err := s.getPromResults(promQuery)

			if err != nil {
				log.Errorf("error querying Prometheus for query %v: %v\n", promQuery, err)
				anyerr = err
				continue
			}

			curMetricMap := s.promResults2MetricMap(promResults, metric, method, window.Duration)
			metricList = append(metricList, curMetricMap[host]...)
		}
	}

	return metricList, anyerr
}

// FetchAllHostsMetrics Fetch all host metrics with different operators (avg_over_time, stddev_over_time) and different resource types (CPU, Memory)
func (s promClient) FetchAllHostsMetrics(window *watcher.Window) (map[string][]watcher.Metric, error) {
	hostMetrics := make(map[string][]watcher.Metric)
	var anyerr error

	for _, method := range []string{promAvg, promStd} {
		for _, metric := range []string{promCpuMetric, promMemMetric, promTransBandMetric, promTransBandDropMetric, promRecBandMetric, promRecBandDropMetric, promDiskIOMetric} {
			promQuery := s.buildPromQuery(allHosts, metric, method, window.Duration)
			promResults, err := s.getPromResults(promQuery)

			if err != nil {
				log.Errorf("error querying Prometheus for query %v: %v\n", promQuery, err)
				anyerr = err
				continue
			}

			curMetricMap := s.promResults2MetricMap(promResults, metric, method, window.Duration)

			for k, v := range curMetricMap {
				hostMetrics[k] = append(hostMetrics[k], v...)
			}
		}
	}

	return hostMetrics, anyerr
}

func (s promClient) Health() (int, error) {
	req, err := http.NewRequest("HEAD", DefaultPromAddress, nil)
	if err != nil {
		return -1, err
	}
	resp, _, err := s.client.Do(context.Background(), req)
	if err != nil {
		return -1, err
	}
	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("received response status code: %v", resp.StatusCode)
	}
	return 0, nil
}

func (s promClient) buildPromQuery(host string, metric string, method string, rollup string) string {
	var promQuery string

	if host == allHosts {
		promQuery = fmt.Sprintf("%s(%s[%s])", method, metric, rollup)
	} else {
		promQuery = fmt.Sprintf("%s(%s{%s=\"%s\"}[%s])", method, metric, hostMetricKey, host, rollup)
	}

	return promQuery
}

func (s promClient) getPromResults(promQuery string) (model.Value, error) {
	v1api := v1.NewAPI(s.client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results, warnings, err := v1api.Query(ctx, promQuery, time.Now())
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		log.Warnf("Warnings: %v\n", warnings)
	}
	log.Debugf("result:\n%v\n", results)

	return results, nil
}

func (s promClient) promResults2MetricMap(promresults model.Value, metric string, method string, rollup string) map[string][]watcher.Metric {
	var metricType string
	var operator string

	curMetrics := make(map[string][]watcher.Metric)

	if metric == promCpuMetric {
		metricType = watcher.CPU
	} else if metric == promMemMetric {
		metricType = watcher.Memory
	} else if metric == promDiskIOMetric {
		metricType = watcher.Storage
	} else {
		metricType = watcher.Bandwidth
	}

	if method == promAvg {
		operator = watcher.Average
	} else if method == promStd {
		operator = watcher.Std
	} else {
		operator = watcher.UnknownOperator
	}

	switch promresults.(type) {
	case model.Vector:
		for _, result := range promresults.(model.Vector) {
			curMetric := watcher.Metric{Name: metric, Type: metricType, Operator: operator, Rollup: rollup, Value: float64(result.Value * 100)}
			curHost := string(result.Metric[hostMetricKey])
			curMetrics[curHost] = append(curMetrics[curHost], curMetric)
		}
	default:
		log.Errorf("error: The Prometheus results should not be type: %v.\n", promresults.Type())
	}

	return curMetrics
}
