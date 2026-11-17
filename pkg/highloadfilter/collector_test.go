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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	"k8s.io/klog/v2"
)

type fakeWatcherClient struct {
	mu      sync.Mutex
	metrics *watcher.WatcherMetrics
	err     error
	calls   int
	called  chan struct{}
}

func (f *fakeWatcherClient) GetLatestWatcherMetrics() (*watcher.WatcherMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	return f.metrics, f.err
}

func TestCollectorRefreshKeepsLastSuccessfulSnapshot(t *testing.T) {
	initial := metricsSnapshot(testNow, 10, 20)
	client := &fakeWatcherClient{metrics: initial}
	c := &collector{client: client}

	if err := c.refresh(); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if got := c.Snapshot(); got != initial {
		t.Fatalf("Snapshot() = %p, want %p", got, initial)
	}

	client.mu.Lock()
	client.metrics = nil
	client.err = errors.New("provider unavailable")
	client.mu.Unlock()
	if err := c.refresh(); err == nil {
		t.Fatal("refresh() error = nil, want provider error")
	}
	if got := c.Snapshot(); got != initial {
		t.Fatalf("Snapshot() after failed refresh = %p, want last successful snapshot %p", got, initial)
	}
}

func TestCollectorStartStopsWithContext(t *testing.T) {
	client := &fakeWatcherClient{
		metrics: metricsSnapshot(testNow, 10, 20),
		called:  make(chan struct{}, 10),
	}
	c := &collector{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx, klog.Background(), time.Millisecond)

	select {
	case <-client.called:
	case <-time.After(time.Second):
		t.Fatal("collector did not refresh before timeout")
	}
	cancel()

	client.mu.Lock()
	callsAfterCancel := client.calls
	client.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.calls > callsAfterCancel+1 {
		t.Fatalf("collector continued refreshing after cancellation: calls before=%d after=%d", callsAfterCancel, client.calls)
	}
}
