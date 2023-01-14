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

package loadvariationriskbalancing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/v1beta2"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

var _ framework.SharedLister = &testSharedLister{}

type testSharedLister struct {
	nodes       []*v1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func (f *testSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *testSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *testSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *testSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

func TestNew(t *testing.T) {
	watcherResponse := watcher.WatcherMetrics{}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	loadVariationRiskBalancingArgs := pluginConfig.LoadVariationRiskBalancingArgs{
		TrimaranSpec:            pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
		SafeVarianceMargin:      v1beta2.DefaultSafeVarianceMargin,
		SafeVarianceSensitivity: v1beta2.DefaultSafeVarianceSensitivity,
	}
	loadVariationRiskBalancingConfig := config.PluginConfig{
		Name: Name,
		Args: &loadVariationRiskBalancingArgs,
	}
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterScorePlugin(Name, New, 1),
	}

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nil)
	fh, err := testutil.NewFramework(registeredPlugins, []config.PluginConfig{loadVariationRiskBalancingConfig},
		"default-scheduler", runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.Nil(t, err)
	p, err := New(&loadVariationRiskBalancingArgs, fh)
	assert.NotNil(t, p)
	assert.Nil(t, err)

	// bad arguments will be substituted by default values
	badArgs := pluginConfig.LoadVariationRiskBalancingArgs{
		TrimaranSpec:       pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
		SafeVarianceMargin: -5,
	}
	badp, err := New(&badArgs, fh)
	assert.NotNil(t, badp)
	assert.Nil(t, err)

	badArgs.SafeVarianceSensitivity = -1
	badp, err = New(&badArgs, fh)
	assert.NotNil(t, badp)
	assert.Nil(t, err)
}

func TestScore(t *testing.T) {

	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

	var mega int64 = 1024 * 1024

	tests := []struct {
		test            string
		pod             *v1.Pod
		nodes           []*v1.Node
		watcherResponse watcher.WatcherMetrics
		expected        framework.NodeScoreList
	}{
		{
			test: "new node",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metric{
								{
									Type:     watcher.CPU,
									Operator: watcher.Average,
									Value:    50,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 75},
			},
		},
		{
			test: "hot node",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{

				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metric{
								{
									Type:     watcher.CPU,
									Operator: watcher.Average,
									Value:    100,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 50},
			},
		},
		{
			test: "average and stDev metrics",
			pod:  getPodWithContainersAndOverhead(0, 0, 0, []int64{200}, []int64{256 * mega}),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metric{
								{
									Type:     watcher.CPU,
									Operator: watcher.Average,
									Value:    30,
								},
								{
									Type:     watcher.CPU,
									Operator: watcher.Std,
									Value:    16,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 67},
			},
		},
		{
			test: "CPU and Memory metrics",
			pod:  getPodWithContainersAndOverhead(0, 0, 0, []int64{100}, []int64{512 * mega}),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metric{
								{
									Type:     watcher.CPU,
									Operator: watcher.Average,
									Value:    40,
								},
								{
									Type:     watcher.CPU,
									Operator: watcher.Std,
									Value:    16,
								},
								{
									Type:     watcher.Memory,
									Operator: watcher.Average,
									Value:    50,
								},
								{
									Type:     watcher.Memory,
									Operator: watcher.Std,
									Value:    10,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 45},
			},
		},
		{
			test: "pick worst case: CPU or Memory",
			pod:  getPodWithContainersAndOverhead(0, 0, 0, []int64{100}, []int64{512 * mega}),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
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
									Value:    20,
								},
								{
									Type:     watcher.Memory,
									Operator: watcher.Average,
									Value:    25,
								},
								{
									Type:     watcher.Memory,
									Operator: watcher.Std,
									Value:    15,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 45},
			},
		},
		{
			test: "404 resp from watcher",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
	}

	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
	}

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(tt.watcherResponse)
				assert.Nil(t, err)
				resp.Write(bytes)
			}))
			defer server.Close()

			nodes := append([]*v1.Node{}, tt.nodes...)
			state := framework.NewCycleState()

			loadVariationRiskBalancingArgs := pluginConfig.LoadVariationRiskBalancingArgs{
				TrimaranSpec:            pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
				SafeVarianceMargin:      v1beta2.DefaultSafeVarianceMargin,
				SafeVarianceSensitivity: v1beta2.DefaultSafeVarianceSensitivity,
			}
			loadVariationRiskBalancingConfig := config.PluginConfig{
				Name: Name,
				Args: &loadVariationRiskBalancingArgs,
			}

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, nodes)

			fh, err := testutil.NewFramework(registeredPlugins, []config.PluginConfig{loadVariationRiskBalancingConfig},
				"default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			p, _ := New(&loadVariationRiskBalancingArgs, fh)
			scorePlugin := p.(framework.ScorePlugin)

			var actualList framework.NodeScoreList
			for _, n := range tt.nodes {
				nodeName := n.Name
				score, status := scorePlugin.Score(context.Background(), state, tt.pod, nodeName)
				assert.True(t, status.IsSuccess())
				actualList = append(actualList, framework.NodeScore{Name: nodeName, Score: score})
			}
			assert.EqualValues(t, tt.expected, actualList)
		})
	}
}

func newTestSharedLister(pods []*v1.Pod, nodes []*v1.Node) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	nodeInfos := make([]*framework.NodeInfo, 0)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		if _, ok := nodeInfoMap[node.Name]; !ok {
			nodeInfoMap[node.Name] = framework.NewNodeInfo()
		}
		nodeInfoMap[node.Name].SetNode(node)
	}

	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
	}

	return &testSharedLister{
		nodes:       nodes,
		nodeInfos:   nodeInfos,
		nodeInfoMap: nodeInfoMap,
	}
}

// getPodWithContainersAndOverhead : length of contCPUReq and contMemReq should be same
func getPodWithContainersAndOverhead(overhead int64, initCPUReq int64, initMemReq int64,
	contCPUReq []int64, contMemReq []int64) *v1.Pod {

	newPod := st.MakePod()
	newPod.Spec.Overhead = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.Overhead[v1.ResourceCPU] = *resource.NewMilliQuantity(overhead, resource.DecimalSI)

	newPod.Spec.InitContainers = []v1.Container{
		{Name: "test-init"},
	}
	newPod.Spec.InitContainers[0].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.InitContainers[0].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(initCPUReq, resource.DecimalSI)
	newPod.Spec.InitContainers[0].Resources.Requests[v1.ResourceMemory] = *resource.NewQuantity(initMemReq, resource.DecimalSI)

	for i := 0; i < len(contCPUReq); i++ {
		newPod.Container("test-container-" + strconv.Itoa(i))
	}
	for i, request := range contCPUReq {
		newPod.Spec.Containers[i].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceMemory] = *resource.NewQuantity(contMemReq[i], resource.DecimalSI)

		newPod.Spec.Containers[i].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceMemory] = *resource.NewQuantity(contMemReq[i], resource.DecimalSI)
	}
	return newPod.Obj()
}
