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
package lowriskovercommitment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	schedConfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
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

func TestLowRiskOverCommitment_New(t *testing.T) {
	watcherResponse := watcher.WatcherMetrics{}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(watcherResponse)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lowRiskOverCommitmentArgs := pluginConfig.LowRiskOverCommitmentArgs{
		TrimaranSpec:        pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
		SmoothingWindowSize: 5,
		RiskLimitWeights: map[v1.ResourceName]float64{
			"cpu":    0.5,
			"memory": 0.5,
		},
	}

	lowRiskOverCommitmentConfig := schedConfig.PluginConfig{
		Name: Name,
		Args: &lowRiskOverCommitmentArgs,
	}

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterPreScorePlugin(Name, New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nil)
	fh, err := testutil.NewFramework(ctx, registeredPlugins, []schedConfig.PluginConfig{lowRiskOverCommitmentConfig},
		"default-scheduler", runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.Nil(t, err)
	p, err := New(ctx, &lowRiskOverCommitmentArgs, fh)
	assert.NotNil(t, p)
	assert.Nil(t, err)

	// bad arguments will be substituted by default values
	badArgs := pluginConfig.LowRiskOverCommitmentArgs{
		TrimaranSpec:        pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
		SmoothingWindowSize: -5,
	}
	badp, err := New(ctx, &badArgs, fh)
	assert.NotNil(t, badp)
	assert.Nil(t, err)

	badArgs.RiskLimitWeights = nil
	badp, err = New(ctx, &badArgs, fh)
	assert.NotNil(t, badp)
	assert.Nil(t, err)
}

func TestLowRiskOverCommitment_Score(t *testing.T) {

	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

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
									Value:    20,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 0},
			},
		},
	}

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
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

			lowRiskOverCommitmentArgs := pluginConfig.LowRiskOverCommitmentArgs{
				TrimaranSpec:        pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
				SmoothingWindowSize: 5,
				RiskLimitWeights: map[v1.ResourceName]float64{
					"cpu":    0.5,
					"memory": 0.5,
				},
			}
			LowRiskOverCommitmentConfig := schedConfig.PluginConfig{
				Name: Name,
				Args: &lowRiskOverCommitmentArgs,
			}

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, nodes)

			fh, err := testutil.NewFramework(ctx, registeredPlugins, []schedConfig.PluginConfig{LowRiskOverCommitmentConfig},
				"default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			p, _ := New(ctx, &lowRiskOverCommitmentArgs, fh)

			preScorePlugin := p.(framework.PreScorePlugin)
			status := preScorePlugin.PreScore(context.Background(), state, tt.pod, tf.BuildNodeInfos(tt.nodes))
			assert.True(t, status.IsSuccess())

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

var plugin_A *LowRiskOverCommitment = &LowRiskOverCommitment{
	handle:    nil,
	collector: nil,
	args: &pluginConfig.LowRiskOverCommitmentArgs{
		SmoothingWindowSize: 5,
		RiskLimitWeights: map[v1.ResourceName]float64{
			"cpu":    0.5,
			"memory": 0.5,
		},
	},
	riskLimitWeightsMap: map[v1.ResourceName]float64{
		"cpu":    0.5,
		"memory": 0.5,
	},
}

var nodeResources_A map[v1.ResourceName]string = map[v1.ResourceName]string{
	v1.ResourceCPU:    "4000m",
	v1.ResourceMemory: "4Ki",
}
var node_A *v1.Node = st.MakeNode().Name("node-A").Capacity(nodeResources_A).Obj()

var watcherData_A watcher.Data = watcher.Data{
	NodeMetricsMap: map[string]watcher.NodeMetrics{
		node_A.Name: {
			Metrics: []watcher.Metric{
				{
					Type:     watcher.CPU,
					Operator: watcher.Average,
					Value:    80,
				},
				{
					Type:     watcher.CPU,
					Operator: watcher.Std,
					Value:    0,
				},
				{
					Type:     watcher.Memory,
					Operator: watcher.Average,
					Value:    25,
				},
				{
					Type:     watcher.Memory,
					Operator: watcher.Std,
					Value:    0,
				},
			},
		},
	},
}

var nrla_A1 *trimaran.NodeRequestsAndLimits = &trimaran.NodeRequestsAndLimits{
	NodeRequest: &framework.Resource{
		MilliCPU: 2000,
		Memory:   2048,
	},
	NodeLimit: &framework.Resource{
		MilliCPU: 3000,
		Memory:   6144,
	},
	NodeRequestMinusPod: &framework.Resource{
		MilliCPU: 1000,
		Memory:   0,
	},
	NodeLimitMinusPod: &framework.Resource{
		MilliCPU: 2000,
		Memory:   0,
	},
	Nodecapacity: &framework.Resource{
		MilliCPU: 4000,
		Memory:   4096,
	},
}

var nrla_A2 *trimaran.NodeRequestsAndLimits = &trimaran.NodeRequestsAndLimits{
	NodeRequest: &framework.Resource{
		MilliCPU: 4000,
		Memory:   1024,
	},
	NodeLimit: &framework.Resource{
		MilliCPU: 5000,
		Memory:   7168,
	},
	NodeRequestMinusPod: &framework.Resource{
		MilliCPU: 3000,
		Memory:   512,
	},
	NodeLimitMinusPod: &framework.Resource{
		MilliCPU: 4000,
		Memory:   6144,
	},
	Nodecapacity: &framework.Resource{
		MilliCPU: 4000,
		Memory:   4096,
	},
}

func TestLowRiskOverCommitment_computeRisk(t *testing.T) {
	tests := []struct {
		name                  string
		resourceName          v1.ResourceName
		resourceType          string
		nodeRequestsAndLimits *trimaran.NodeRequestsAndLimits
		want                  float64
	}{
		{
			name:                  "test-cpu-1",
			resourceName:          v1.ResourceCPU,
			resourceType:          watcher.CPU,
			nodeRequestsAndLimits: nrla_A1,
			want:                  0.5,
		},
		{
			name:                  "test-mem-1",
			resourceName:          v1.ResourceMemory,
			resourceType:          watcher.Memory,
			nodeRequestsAndLimits: nrla_A1,
			want:                  0.25,
		},
		{
			name:                  "test-cpu-2",
			resourceName:          v1.ResourceCPU,
			resourceType:          watcher.CPU,
			nodeRequestsAndLimits: nrla_A2,
			want:                  1.0,
		},
		{
			name:                  "test-mem-2",
			resourceName:          v1.ResourceMemory,
			resourceType:          watcher.Memory,
			nodeRequestsAndLimits: nrla_A2,
			want:                  0.75,
		},
	}
	pl := &LowRiskOverCommitment{
		handle:              plugin_A.handle,
		collector:           plugin_A.collector,
		args:                plugin_A.args,
		riskLimitWeightsMap: plugin_A.riskLimitWeightsMap,
	}
	metrics := watcherData_A.NodeMetricsMap[node_A.Name].Metrics
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pl.computeRisk(metrics, tt.resourceName, tt.resourceType, node_A, tt.nodeRequestsAndLimits); got != tt.want {
				t.Errorf("LowRiskOverCommitment.computeRisk() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newTestSharedLister(pods []*v1.Pod, nodes []*v1.Node) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	var nodeInfos []*framework.NodeInfo
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
