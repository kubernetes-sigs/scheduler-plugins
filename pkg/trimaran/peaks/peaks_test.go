/*
Copyright 2020 The Kubernetes Authors.

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

package peaks

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"

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
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	testutil2 "sigs.k8s.io/scheduler-plugins/test/integration"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
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

func createSamplePowerModel() {
	data := map[string]interface{}{
		"node-1": map[string]float64{
			"k0": 471.7412504314313,
			"k1": -91.50493019588365,
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
		os.Setenv("NODE_POWER_MODEL", "./power_model/node_power_model")
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

func createErroredPowerModel() {
	data := map[string]interface{}{
		"node-1": 10,
	}
	powerModelData, err := json.Marshal(data)
	if err != nil {
		fmt.Println("Json marshal error: ", err)
		return
	}

	err = ioutil.WriteFile(os.Getenv("NODE_POWER_MODEL"), powerModelData, 0644)
	if err != nil {
		fmt.Println("Json file write error: ", err)
		return
	}
}

func TestPeaksNew(t *testing.T) {
	// Create a sample power model
	createSamplePowerModel()

	watcherResponse := watcher.WatcherMetrics{}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peaksArgs := pluginConfig.PeaksArgs{
		WatcherAddress: server.URL,
	}
	peaksConfig := config.PluginConfig{
		Name: Name,
		Args: &peaksArgs,
	}
	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nil)
	fh, err := testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{peaksConfig},
		"kube-scheduler", runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.Nil(t, err)
	p, err := New(ctx, &peaksArgs, fh)
	assert.NotNil(t, p)
	assert.Nil(t, err)

	peaksConfig = config.PluginConfig{
		Name: Name,
		Args: nil,
	}
	fh, err = testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{peaksConfig},
		"kube-scheduler", runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.NotNil(t, err)
	assert.EqualError(t, err, "initializing plugin \"Peaks\": "+"want args to be of type PeaksArgs, got <nil>")

	// Check that the default power model is returned, if a power model doesn't exist for a node
	defaultPowerModel := PowerModel{
		K0: 0,
		K1: 0,
		K2: 0,
	}
	err = initNodePowerModels()
	if err != nil {
		assert.Nil(t, err)
	}
	nodePowerModel := getPowerModel("node-2")
	assert.EqualValues(t, nodePowerModel, defaultPowerModel)

	// Check by setting the env variable NODE_POWER_MODEL to an invalid path
	envVarNodePowerModel := os.Getenv("NODE_POWER_MODEL")
	os.Setenv("NODE_POWER_MODEL", envVarNodePowerModel+"/invalid")
	fmt.Println("updated Path: ", os.Getenv("NODE_POWER_MODEL"))
	err = initNodePowerModels()
	assert.NotNil(t, err)
	assert.EqualError(t, err, "open "+os.Getenv("NODE_POWER_MODEL")+": not a directory")
	os.Setenv("NODE_POWER_MODEL", envVarNodePowerModel)

	// Check by updating the sample power model to wrong format
	createErroredPowerModel()
	err = initNodePowerModels()
	assert.NotNil(t, err)
	assert.EqualError(t, err, "json: cannot unmarshal number into Go value of type peaks.PowerModel")
	os.Setenv("NODE_POWER_MODEL", envVarNodePowerModel)

	// Delete the sample power model
	deleteSamplePowerModel()
}

func TestPeaksScore(t *testing.T) {
	// Create a sample power model
	createSamplePowerModel()

	watcherResponse := watcher.WatcherMetrics{}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	peaksArgs := pluginConfig.PeaksArgs{
		WatcherAddress: server.URL,
	}
	peaksConfig := config.PluginConfig{
		Name: Name,
		Args: &peaksArgs,
	}

	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

	testPod1 := st.MakePod().Obj()
	testPod1.Spec.Containers = append(testPod1.Spec.Containers, v1.Container{
		Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(1000, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewMilliQuantity(2000, resource.DecimalSI),
			},
		},
	})
	testPod2 := st.MakePod().Obj()
	testPod2.Spec.Containers = append(testPod2.Spec.Containers, v1.Container{
		Resources: v1.ResourceRequirements{},
	})

	testPod3 := st.MakePod().Obj()
	testPod3.Spec.Overhead = v1.ResourceList{
		v1.ResourceCPU: testPod1.Spec.Containers[0].Resources.Requests[v1.ResourceCPU],
	}

	testPod4 := st.MakePod().Obj()
	testPod4.Spec.Containers = append(testPod4.Spec.Containers, v1.Container{
		Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				v1.ResourceCPU: *resource.NewMilliQuantity(2000, resource.DecimalSI),
			},
		},
	})

	err := initNodePowerModels()
	if err != nil {
		assert.Nil(t, err)
	}
	jumpInPower := getPowerJumpForUtilisation(0, 100, getPowerModel("node-1"))
	scoreToUse := int64(jumpInPower * math.Pow(10, 15))

	tests := []struct {
		test            string
		pod             *v1.Pod
		nodes           []*v1.Node
		watcherResponse watcher.WatcherMetrics
		expected        framework.NodeScoreList
	}{
		{
			test: "Pod with Requests",
			pod:  testutil2.MakePod("ns", "p").Container(testutil2.MakeResourceList().CPU(1).Mem(2).Obj()).Obj(),
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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: scoreToUse},
			},
		},
		{
			test: "Pod with Limits",
			pod:  testPod1,
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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: scoreToUse},
			},
		},
		{
			test: "Pod with no Requirements",
			pod:  testPod2,
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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: scoreToUse},
			},
		},
		{
			test: "No CPU metrics found",
			pod:  testutil2.MakePod("ns", "p").Container(testutil2.MakeResourceList().CPU(1).Mem(2).Obj()).Obj(),
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
									Type:     watcher.Memory,
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
		{
			test: "Pod with Overhead",
			//pod:  testutil2.MakePod("ns", "p").Container(testutil2.MakeResourceList().CPU(1).Mem(2).Obj()).Overhead(testutil2.MakeResourceList().CPU(1).Obj()).Obj(),
			pod: testPod3,
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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
		{
			test: "Pod with above node resource capacity",
			pod:  testPod4,
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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
		{
			test: "No watcher response for node",
			pod:  testPod4,
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
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

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(tt.watcherResponse)
				assert.Nil(t, err)
				resp.Write(bytes)
			}))
			defer server.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			nodes := append([]*v1.Node{}, tt.nodes...)
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, nodes)
			fh, err := testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{peaksConfig},
				"default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			peaksArgs := pluginConfig.PeaksArgs{
				WatcherAddress: server.URL,
			}
			p, _ := New(ctx, &peaksArgs, fh)
			scorePlugin := p.(framework.ScorePlugin)
			var actualList framework.NodeScoreList
			for _, n := range tt.nodes {
				nodeName := n.Name
				score, status := scorePlugin.Score(context.Background(), state, tt.pod, nodeName)
				assert.True(t, status.IsSuccess())
				actualList = append(actualList, framework.NodeScore{Name: nodeName, Score: score})
			}
			assert.ElementsMatch(t, tt.expected, actualList)
		})
	}

	// Delete the sample power model
	deleteSamplePowerModel()
}

func TestPeaksNormalizeScore(t *testing.T) {
	// Create a sample power model
	createSamplePowerModel()

	watcherResponse := watcher.WatcherMetrics{}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterPluginAsExtensions(Name, New, "Score"),
	}

	peaksArgs := pluginConfig.PeaksArgs{
		WatcherAddress: server.URL,
	}
	peaksConfig := config.PluginConfig{
		Name: Name,
		Args: &peaksArgs,
	}

	nodeScoreList1 := []framework.NodeScore{
		{Name: "node-1", Score: framework.MinNodeScore},
		{Name: "node-2", Score: framework.MaxNodeScore},
	}

	nodeScoreList2 := []framework.NodeScore{
		{Name: "node-1", Score: framework.MinNodeScore},
		{Name: "node-2", Score: framework.MinNodeScore},
	}

	nodeScoreList3 := []framework.NodeScore{
		{Name: "node-1", Score: framework.MaxNodeScore},
		{Name: "node-2", Score: framework.MaxNodeScore},
	}

	tests := []struct {
		test            string
		pod             *v1.Pod
		watcherResponse watcher.WatcherMetrics
		nodeScoreList   framework.NodeScoreList
		expected        framework.NodeScoreList
	}{
		{
			test:          "Normalize score {minScore, maxScore}",
			pod:           st.MakePod().Obj(),
			nodeScoreList: nodeScoreList1,
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MaxNodeScore},
				{Name: "node-2", Score: framework.MinNodeScore},
			},
		},
		{
			test:          "Normalize score {minScore, minScore}",
			pod:           st.MakePod().Obj(),
			nodeScoreList: nodeScoreList2,
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
				{Name: "node-2", Score: framework.MinNodeScore},
			},
		},
		{
			test:          "Normalize score {maxScore, maxScore}",
			pod:           st.MakePod().Obj(),
			nodeScoreList: nodeScoreList3,
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MaxNodeScore},
				{Name: "node-2", Score: framework.MaxNodeScore},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			nodes := []*v1.Node{}
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, nodes)
			fh, err := testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{peaksConfig},
				"default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)

			p, _ := New(ctx, &peaksArgs, fh)
			scorePlugin := p.(framework.ScorePlugin)
			status := scorePlugin.ScoreExtensions().NormalizeScore(context.Background(), state, tt.pod, tt.nodeScoreList)
			assert.True(t, status.IsSuccess())
			assert.ElementsMatch(t, tt.expected, tt.nodeScoreList)
		})
	}

	//Delete sample power model
	deleteSamplePowerModel()
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
