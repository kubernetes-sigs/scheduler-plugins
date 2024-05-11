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
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
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

	"sigs.k8s.io/scheduler-plugins/apis/config"
	cfgv1 "sigs.k8s.io/scheduler-plugins/apis/config/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/loadvariationriskbalancing"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestLoadVariationRiskBalancingPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	metrics := watcher.WatcherMetrics{
		Timestamp: 1556987522,
		Window: watcher.Window{
			Duration: "15m",
			Start:    1556984522,
			End:      1556985422,
		},
		Data: watcher.Data{
			NodeMetricsMap: map[string]watcher.NodeMetrics{
				"node-1": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    30,
							Operator: watcher.Average,
						},
					},
				},
				"node-2": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    70,
							Operator: watcher.Average,
						},
						{
							Type:     watcher.CPU,
							Operator: watcher.Std,
							Value:    20,
						},
					},
				},
				"node-3": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    40,
							Operator: watcher.Average,
						},
						{
							Type:     watcher.CPU,
							Operator: watcher.Std,
							Value:    30,
						},
					},
				},
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(metrics)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Work around https://github.com/kubernetes/kubernetes/issues/121630.
	cfg.Profiles[0].Plugins.PreScore = schedapi.PluginSet{
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
		Enabled: []schedapi.Plugin{
			{Name: loadvariationriskbalancing.Name},
		},
		Disabled: []schedapi.Plugin{
			{Name: "*"},
		},
	}
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: loadvariationriskbalancing.Name,
		Args: &config.LoadVariationRiskBalancingArgs{
			TrimaranSpec:       config.TrimaranSpec{WatcherAddress: server.URL},
			SafeVarianceMargin: cfgv1.DefaultSafeVarianceMargin,
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)
	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{loadvariationriskbalancing.Name: loadvariationriskbalancing.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	var nodes []*v1.Node
	nodeNames := []string{"node-1", "node-2", "node-3"}
	capacity := map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "256",
	}
	for i := 0; i < len(nodeNames); i++ {
		node := st.MakeNode().Name(nodeNames[i]).Label("node", nodeNames[i]).Capacity(capacity).Obj()
		node, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
		assert.Nil(t, err)
		nodes = append(nodes, node)
	}

	var newPods []*v1.Pod
	podNames := []string{"pod-1", "pod-2"}
	containerCPU := []int64{300, 100}
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Obj()
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(containerCPU[i], resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(50, resource.DecimalSI),
			},
		}
		newPods = append(newPods, pod)
	}

	for i := range newPods {
		t.Logf("Creating Pod %q", newPods[i].Name)
		_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, newPods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", newPods[i].Name, err)
		}
	}
	defer cleanupPods(t, testCtx, newPods)

	expected := [2]string{"node-1", "node-1"}
	for i := range newPods {
		err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 10*time.Second, false, func(ctx context.Context) (bool, error) {
			return podScheduled(cs, newPods[i].Namespace, newPods[i].Name), nil
		})
		assert.Nil(t, err)

		pod, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, newPods[i].Name, metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, expected[i], pod.Spec.NodeName)
	}

}
