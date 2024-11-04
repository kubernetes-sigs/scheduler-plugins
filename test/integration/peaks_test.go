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
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/peaks"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func createSamplePowerModel() {
	data := map[string]interface{}{
		"node-1": map[string]float64{
			"k0": 471.7412504314313,
			"k1": -91.50493019588365,
			"k2": -0.07186049052516228,
		},
		"node-2": map[string]float64{
			"k0": 471.7412504314313,
			"k1": -1091.50493019588365,
			"k2": -0.07186049052516228,
		},
		"node-3": map[string]float64{
			"k0": 471.7412504314313,
			"k1": -2091.50493019588365,
			"k2": -0.07186049052516228,
		},
	}
	fmt.Println("Power model json data: ", data)
	powerModelData, err := json.Marshal(data)
	if err != nil {
		fmt.Println("Json marshal error: ", err)
		return
	}

	if len(os.Getenv("NODE_POWER_MODEL")) == 0 {
		os.Setenv("NODE_POWER_MODEL", "/tmp/power_model/node_power_model")
	}
	fmt.Println("NODE_POWER_MODEL: ", os.Getenv("NODE_POWER_MODEL"))
	fileDir, fileName := filepath.Split(os.Getenv("NODE_POWER_MODEL"))
	fmt.Println("fileDir: ", fileDir, "\nfileName: ", fileName)
	err = os.MkdirAll(fileDir, 0777)
	if err != nil {
		fmt.Println("Json file directory create error: ", err)
		return
	}
	err = ioutil.WriteFile(os.Getenv("NODE_POWER_MODEL"), powerModelData, 0644)
	if err != nil {
		fmt.Println("Json file write error: ", err)
		return
	}
}

func deleteSamplePowerModel() {
	fileDir, fileName := filepath.Split(os.Getenv("NODE_POWER_MODEL"))
	fmt.Println("fileDir: ", fileDir, "\nfileName: ", fileName)
	err := os.RemoveAll(fileDir)
	if err != nil {
		fmt.Println("Delete file error: ", err)
		return
	}
}

func TestPeaksPlugin(t *testing.T) {
	// Create a sample power model
	createSamplePowerModel()

	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	metrics := watcher.WatcherMetrics{
		Window: watcher.Window{},
		Data: watcher.Data{
			NodeMetricsMap: map[string]watcher.NodeMetrics{
				"node-1": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    0,
							Operator: watcher.Latest,
						},
					},
				},
				"node-2": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    0,
							Operator: watcher.Latest,
						},
					},
				},
				"node-3": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    0,
							Operator: watcher.Latest,
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
			{Name: peaks.Name},
		},
		Disabled: []schedapi.Plugin{
			{Name: "*"},
		},
	}
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: peaks.Name,
		Args: &config.PeaksArgs{
			WatcherAddress: server.URL,
		},
	})

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{peaks.Name: peaks.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	var nodes []*v1.Node
	nodeNames := []string{"node-1", "node-2", "node-3"}
	for i := 0; i < len(nodeNames); i++ {
		node := st.MakeNode().Name(nodeNames[i]).Label("node", nodeNames[i]).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}
		node, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
		assert.Nil(t, err)
		nodes = append(nodes, node)
	}

	var newPods []*v1.Pod
	podNames := []string{"pod-1", "pod-2"}
	containerCPU := []int64{300, 1900}
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

	expected := [2]string{nodeNames[0], nodeNames[1]}
	for i := range newPods {
		err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 10*time.Second, false, func(ctx context.Context) (bool, error) {
			return podScheduled(cs, newPods[i].Namespace, newPods[i].Name), nil
		})
		assert.Nil(t, err)

		pod, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, newPods[i].Name, metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, expected[i], pod.Spec.NodeName)
	}

	// Delete the sample power model
	deleteSamplePowerModel()
}
