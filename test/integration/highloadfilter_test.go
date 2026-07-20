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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/highloadfilter"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestHighLoadFilterPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())
	client := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = client
	testCtx.KubeConfig = globalKubeConfig

	metrics := watcher.WatcherMetrics{
		Timestamp: time.Now().Unix(),
		Data: watcher.Data{NodeMetricsMap: watcher.NodeMetricsMap{
			"low-load-node": {Metrics: []watcher.Metric{
				{Type: watcher.CPU, Operator: watcher.Average, Value: 20},
				{Type: watcher.Memory, Operator: watcher.Average, Value: 20},
			}},
			"high-load-node": {Metrics: []watcher.Metric{
				{Type: watcher.CPU, Operator: watcher.Average, Value: 70},
				{Type: watcher.Memory, Operator: watcher.Average, Value: 20},
			}},
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, _ *http.Request) {
		resp.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(resp).Encode(metrics); err != nil {
			t.Errorf("encode watcher response: %v", err)
		}
	}))
	defer server.Close()

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled,
		schedapi.Plugin{Name: highloadfilter.Name})
	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled,
		schedapi.Plugin{Name: highloadfilter.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: highloadfilter.Name,
		Args: &pluginconfig.HighLoadFilterArgs{
			WatcherAddress: server.URL,
			UsageThresholds: pluginconfig.HighLoadFilterUsageThresholds{
				CPU:    60,
				Memory: 90,
			},
			FailOpen:                     false,
			MetricsUpdateIntervalSeconds: 30,
			NodeMetricExpirationSeconds:  180,
		},
	})

	namespace := fmt.Sprintf("integration-test-%s", uuid.NewUUID())
	createNamespace(t, testCtx, namespace)
	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{highloadfilter.Name: highloadfilter.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	capacity := map[corev1.ResourceName]string{
		corev1.ResourceCPU:    "1",
		corev1.ResourceMemory: "1Gi",
	}
	for _, name := range []string{"low-load-node", "high-load-node"} {
		node := st.MakeNode().Name(name).Capacity(capacity).Obj()
		if _, err := client.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create node %q: %v", name, err)
		}
	}

	pod := st.MakePod().Namespace(namespace).Name("high-load-filter-pod").Container(imageutils.GetPauseImageName()).Obj()
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("100Mi"),
	}
	createdPod, err := client.CoreV1().Pods(namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create Pod: %v", err)
	}
	defer cleanupPods(t, testCtx, []*corev1.Pod{createdPod})

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 10*time.Second, false, func(context.Context) (bool, error) {
		return podScheduled(t, client, namespace, pod.Name), nil
	}); err != nil {
		t.Fatalf("wait for Pod scheduling: %v", err)
	}
	scheduledPod, err := client.CoreV1().Pods(namespace).Get(testCtx.Ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get scheduled Pod: %v", err)
	}
	if scheduledPod.Spec.NodeName != "low-load-node" {
		t.Fatalf("Pod scheduled on %q, want low-load-node", scheduledPod.Spec.NodeName)
	}
}
