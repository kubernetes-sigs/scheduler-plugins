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
	"math"
	"testing"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

var testNow = time.Unix(2_000_000_000, 0)

type staticMetricsSource struct {
	metrics *watcher.WatcherMetrics
}

func (s *staticMetricsSource) Snapshot() *watcher.WatcherMetrics {
	return s.metrics
}

func TestFilter(t *testing.T) {
	tests := []struct {
		name       string
		metrics    *watcher.WatcherMetrics
		failOpen   bool
		thresholds pluginconfig.HighLoadFilterUsageThresholds
		podCPU     string
		podMemory  string
		wantCode   fwk.Code
		wantReason string
	}{
		{
			name:       "passes below thresholds",
			metrics:    metricsSnapshot(testNow, 30, 40),
			failOpen:   false,
			thresholds: thresholds(60, 60),
			podCPU:     "100m",
			podMemory:  "100Mi",
			wantCode:   fwk.Success,
		},
		{
			name:       "allows utilization equal to threshold",
			metrics:    metricsSnapshot(testNow, 50, 50),
			failOpen:   false,
			thresholds: thresholds(60, 60),
			podCPU:     "100m",
			podMemory:  "100Mi",
			wantCode:   fwk.Success,
		},
		{
			name:       "rejects CPU measured above threshold",
			metrics:    metricsSnapshot(testNow, 70, 30),
			failOpen:   true,
			thresholds: thresholds(60, 90),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonCPULoadExceeds,
		},
		{
			name:       "rejects CPU when Pod request crosses threshold",
			metrics:    metricsSnapshot(testNow, 55, 30),
			failOpen:   true,
			thresholds: thresholds(60, 90),
			podCPU:     "100m",
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonCPULoadExceeds,
		},
		{
			name:       "rejects memory when Pod request crosses threshold",
			metrics:    metricsSnapshot(testNow, 20, 55),
			failOpen:   true,
			thresholds: thresholds(90, 60),
			podMemory:  "100Mi",
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonMemoryLoadExceeds,
		},
		{
			name:       "fails open when snapshot is unavailable",
			metrics:    nil,
			failOpen:   true,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Success,
		},
		{
			name:       "fails closed when snapshot is unavailable",
			metrics:    nil,
			failOpen:   false,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonMetricsUnavailable,
		},
		{
			name:       "fails open when snapshot is stale",
			metrics:    metricsSnapshot(testNow.Add(-181*time.Second), 10, 10),
			failOpen:   true,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Success,
		},
		{
			name:       "fails closed when snapshot is stale",
			metrics:    metricsSnapshot(testNow.Add(-181*time.Second), 10, 10),
			failOpen:   false,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonMetricsStale,
		},
		{
			name:       "fails closed when node metrics are missing",
			metrics:    metricsSnapshotForNodes(testNow, watcher.NodeMetricsMap{}),
			failOpen:   false,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonMetricsUnavailable,
		},
		{
			name: "enforces available CPU before failing open for missing memory",
			metrics: metricsSnapshotForNodes(testNow, watcher.NodeMetricsMap{
				"node-1": {Metrics: []watcher.Metric{{Type: watcher.CPU, Operator: watcher.Average, Value: 80}}},
			}),
			failOpen:   true,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonCPULoadExceeds,
		},
		{
			name: "fails open for one missing resource when available resource is safe",
			metrics: metricsSnapshotForNodes(testNow, watcher.NodeMetricsMap{
				"node-1": {Metrics: []watcher.Metric{{Type: watcher.CPU, Operator: watcher.Average, Value: 20}}},
			}),
			failOpen:   true,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Success,
		},
		{
			name: "fails closed for one missing resource",
			metrics: metricsSnapshotForNodes(testNow, watcher.NodeMetricsMap{
				"node-1": {Metrics: []watcher.Metric{{Type: watcher.CPU, Operator: watcher.Average, Value: 20}}},
			}),
			failOpen:   false,
			thresholds: thresholds(60, 60),
			wantCode:   fwk.Unschedulable,
			wantReason: ErrReasonMetricsUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := newTestPlugin(tt.metrics, tt.failOpen, tt.thresholds)
			pod := testPod(tt.podCPU, tt.podMemory)
			state := framework.NewCycleState()
			if _, status := pl.PreFilter(context.Background(), state, pod, nil); !status.IsSuccess() {
				t.Fatalf("PreFilter() status = %v, want success", status)
			}

			status := pl.Filter(context.Background(), state, pod, testNodeInfo())
			if status.Code() != tt.wantCode {
				t.Fatalf("Filter() code = %v, want %v; status: %v", status.Code(), tt.wantCode, status)
			}
			if tt.wantReason != "" && status.Message() != tt.wantReason {
				t.Fatalf("Filter() message = %q, want %q", status.Message(), tt.wantReason)
			}
		})
	}
}

func TestPreFilterUsesOneSnapshotPerCycle(t *testing.T) {
	source := &staticMetricsSource{metrics: metricsSnapshot(testNow, 20, 20)}
	pl := newTestPluginWithSource(source, false, thresholds(60, 60))
	state := framework.NewCycleState()
	pod := testPod("0", "0")
	if _, status := pl.PreFilter(context.Background(), state, pod, nil); !status.IsSuccess() {
		t.Fatalf("PreFilter() status = %v, want success", status)
	}

	// A refresh after PreFilter must not change the decision in this cycle.
	source.metrics = metricsSnapshot(testNow, 90, 90)
	if status := pl.Filter(context.Background(), state, pod, testNodeInfo()); !status.IsSuccess() {
		t.Fatalf("Filter() status = %v, want the captured low-load snapshot", status)
	}
}

func TestFilterRequiresPreFilterState(t *testing.T) {
	pl := newTestPlugin(metricsSnapshot(testNow, 20, 20), false, thresholds(60, 60))
	status := pl.Filter(context.Background(), framework.NewCycleState(), testPod("0", "0"), testNodeInfo())
	if status.Code() != fwk.Error {
		t.Fatalf("Filter() code = %v, want Error", status.Code())
	}
}

func TestFilterHonorsContextCancellation(t *testing.T) {
	pl := newTestPlugin(metricsSnapshot(testNow, 20, 20), false, thresholds(60, 60))
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	status := pl.Filter(ctx, framework.NewCycleState(), testPod("0", "0"), testNodeInfo())
	if status.Code() != fwk.UnschedulableAndUnresolvable {
		t.Fatalf("Filter() code = %v, want UnschedulableAndUnresolvable", status.Code())
	}
}

func TestResourceUsage(t *testing.T) {
	metrics := []watcher.Metric{
		{Type: watcher.CPU, Operator: watcher.Latest, Value: 75},
		{Type: watcher.CPU, Operator: watcher.Average, Value: 45},
		{Type: watcher.Memory, Operator: watcher.Average, Value: math.NaN()},
	}
	if got, ok := resourceUsage(metrics, watcher.CPU); !ok || got != 45 {
		t.Fatalf("resourceUsage(CPU) = (%v, %v), want (45, true)", got, ok)
	}
	if _, ok := resourceUsage(metrics, watcher.Memory); ok {
		t.Fatal("resourceUsage(Memory) found a NaN metric, want false")
	}
}

func TestNewRejectsInvalidArgsBeforeCreatingCollector(t *testing.T) {
	_, err := New(context.Background(), &pluginconfig.HighLoadFilterArgs{
		MetricProvider:               pluginconfig.HighLoadFilterMetricProviderSpec{Type: "invalid"},
		UsageThresholds:              thresholds(101, 100),
		MetricsUpdateIntervalSeconds: 30,
		NodeMetricExpirationSeconds:  180,
	}, nil)
	if err == nil {
		t.Fatal("New() error = nil, want invalid configuration error")
	}

	if _, err := New(context.Background(), &runtime.Unknown{}, nil); err == nil {
		t.Fatal("New() with wrong args type error = nil")
	}
}

func TestNameAndSignPod(t *testing.T) {
	pl := newTestPlugin(metricsSnapshot(testNow, 20, 20), false, thresholds(60, 60))
	if pl.Name() != Name {
		t.Fatalf("Name() = %q, want %q", pl.Name(), Name)
	}
	fragments, status := pl.SignPod(context.Background(), testPod("250m", "200Mi"))
	if !status.IsSuccess() {
		t.Fatalf("SignPod() status = %v, want success", status)
	}
	if len(fragments) != 1 || fragments[0].Key != Name+"/requests" {
		t.Fatalf("SignPod() fragments = %#v, want one requests fragment", fragments)
	}
}

func newTestPlugin(metrics *watcher.WatcherMetrics, failOpen bool, usageThresholds pluginconfig.HighLoadFilterUsageThresholds) *HighLoadFilter {
	return newTestPluginWithSource(&staticMetricsSource{metrics: metrics}, failOpen, usageThresholds)
}

func newTestPluginWithSource(source metricsSource, failOpen bool, usageThresholds pluginconfig.HighLoadFilterUsageThresholds) *HighLoadFilter {
	return &HighLoadFilter{
		logger: klog.Background(),
		source: source,
		args: &pluginconfig.HighLoadFilterArgs{
			UsageThresholds:             usageThresholds,
			FailOpen:                    failOpen,
			NodeMetricExpirationSeconds: 180,
		},
		now: func() time.Time { return testNow },
	}
}

func thresholds(cpu, memory int64) pluginconfig.HighLoadFilterUsageThresholds {
	return pluginconfig.HighLoadFilterUsageThresholds{CPU: cpu, Memory: memory}
}

func metricsSnapshot(timestamp time.Time, cpu, memory float64) *watcher.WatcherMetrics {
	return metricsSnapshotForNodes(timestamp, watcher.NodeMetricsMap{
		"node-1": {Metrics: []watcher.Metric{
			{Type: watcher.CPU, Operator: watcher.Average, Value: cpu},
			{Type: watcher.Memory, Operator: watcher.Average, Value: memory},
		}},
	})
}

func metricsSnapshotForNodes(timestamp time.Time, nodes watcher.NodeMetricsMap) *watcher.WatcherMetrics {
	return &watcher.WatcherMetrics{
		Timestamp: timestamp.Unix(),
		Data:      watcher.Data{NodeMetricsMap: nodes},
	}
}

func testPod(cpu, memory string) *corev1.Pod {
	requests := corev1.ResourceList{}
	if cpu != "" {
		requests[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		requests[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "container",
			Image:     "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Requests: requests},
		}}},
	}
}

func testNodeInfo() fwk.NodeInfo {
	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	})
	return nodeInfo
}
