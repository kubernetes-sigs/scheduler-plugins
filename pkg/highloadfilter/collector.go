/*
Copyright 2026 The Kubernetes Authors.

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

package highloadfilter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	loadwatcherapi "github.com/paypal/load-watcher/pkg/watcher/api"
	"k8s.io/klog/v2"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

type metricsSource interface {
	Snapshot() *watcher.WatcherMetrics
}

type collector struct {
	client loadwatcherapi.Client

	mu      sync.RWMutex
	metrics *watcher.WatcherMetrics
}

func newCollector(logger klog.Logger, args *pluginconfig.HighLoadFilterArgs) (*collector, error) {
	var (
		client loadwatcherapi.Client
		err    error
	)
	if args.WatcherAddress != "" {
		client, err = loadwatcherapi.NewServiceClient(args.WatcherAddress)
	} else {
		client, err = loadwatcherapi.NewLibraryClient(watcher.MetricsProviderOpts{
			Name:               string(args.MetricProvider.Type),
			Address:            args.MetricProvider.Address,
			AuthToken:          args.MetricProvider.Token,
			InsecureSkipVerify: args.MetricProvider.InsecureSkipVerify,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("create load-watcher client: %w", err)
	}

	c := &collector{client: client}
	if err := c.refresh(); err != nil {
		logger.Error(err, "Unable to populate real-load metrics initially")
	}
	return c, nil
}

func (c *collector) Start(ctx context.Context, logger klog.Logger, updateInterval time.Duration) {
	go func() {
		ticker := time.NewTicker(updateInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.refresh(); err != nil {
					logger.Error(err, "Unable to update real-load metrics")
				}
			}
		}
	}()
}

func (c *collector) Snapshot() *watcher.WatcherMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics
}

func (c *collector) refresh() error {
	metrics, err := c.client.GetLatestWatcherMetrics()
	if err != nil {
		return fmt.Errorf("get latest load-watcher metrics: %w", err)
	}
	if metrics == nil {
		return fmt.Errorf("get latest load-watcher metrics: empty response")
	}
	c.mu.Lock()
	c.metrics = metrics
	c.mu.Unlock()
	return nil
}
