/*
Copyright 2023 The Kubernetes Authors.

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

	"sigs.k8s.io/scheduler-plugins/apis/config"
	v1 "sigs.k8s.io/scheduler-plugins/apis/config/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/lowriskovercommitment"

	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestLowRiskOverCommitmentPlugin(t *testing.T) {
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
							Operator: watcher.Average,
							Value:    60,
						},
						{
							Type:     watcher.CPU,
							Operator: watcher.Std,
							Value:    30,
						},
					},
				},
				"node-2": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Operator: watcher.Average,
							Value:    30,
						},
						{
							Type:     watcher.CPU,
							Operator: watcher.Std,
							Value:    20,
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
			{Name: lowriskovercommitment.Name},
		},
		Disabled: []schedapi.Plugin{
			{Name: "*"},
		},
	}
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: lowriskovercommitment.Name,
		Args: &config.LowRiskOverCommitmentArgs{
			TrimaranSpec:        config.TrimaranSpec{WatcherAddress: server.URL},
			SmoothingWindowSize: v1.DefaultSmoothingWindowSize,
			RiskLimitWeights: map[corev1.ResourceName]float64{
				"cpu":    1,
				"memory": 1,
			},
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)
	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{lowriskovercommitment.Name: lowriskovercommitment.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	// Cluster with two nodes
	nodeNames := []string{"node-1", "node-2"}
	capCPU := []string{"2", "2"}
	capMemory := []string{"256", "256"}
	for i := 0; i < len(nodeNames); i++ {
		t.Logf("Creating Node %q", nodeNames[i])
		capacity := map[corev1.ResourceName]string{
			corev1.ResourceCPU:    capCPU[i],
			corev1.ResourceMemory: capMemory[i],
		}
		node := st.MakeNode().Name(nodeNames[i]).Label("node", nodeNames[i]).Capacity(capacity).Obj()
		_, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
		assert.Nil(t, err)
	}

	// Pods divided into numExistingPods already scheduled and the rest to be scheduled
	var pods []*corev1.Pod
	podNames := []string{"pod-1", "pod-2", "pod-3"}
	requestCPU := []int64{500, 100, 500}
	limitCPU := []int64{500, 1200, 1000}
	var requestMemory int64 = 64
	var limitMemory int64 = 64
	scheduledNodes := []string{"node-1", "node-2"}
	numExistingPods := len(scheduledNodes)
	numPods := len(podNames)
	for i := 0; i < numPods; i++ {
		var pod *corev1.Pod
		if i < numExistingPods {
			pod = st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Node(nodeNames[i]).Obj()
		} else {
			pod = st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Obj()
		}
		pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(requestCPU[i], resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(requestMemory, resource.DecimalSI),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(limitCPU[i], resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(limitMemory, resource.DecimalSI),
			},
		}
		pods = append(pods, pod)
	}

	for i := range pods {
		t.Logf("Creating Pod %q", pods[i].Name)
		_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", pods[i].Name, err)
		}
	}
	defer cleanupPods(t, testCtx, pods)

	expectedNodes := []string{"node-1"}
	for i := numExistingPods; i < numPods; i++ {
		err = wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 10*time.Second, false, func(ctx context.Context) (bool, error) {
			return podScheduled(cs, pods[i].Namespace, pods[i].Name), nil
		})
		assert.Nil(t, err)

		pod, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pods[i].Name, metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, expectedNodes[i-numExistingPods], pod.Spec.NodeName)
	}
}
