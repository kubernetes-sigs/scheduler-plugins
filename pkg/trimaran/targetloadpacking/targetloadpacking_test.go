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

package targetloadpacking

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	cfgv1 "sigs.k8s.io/scheduler-plugins/apis/config/v1"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetLoadPackingArgs := pluginConfig.TargetLoadPackingArgs{
		TrimaranSpec:              pluginConfig.TrimaranSpec{WatcherAddress: "http://deadbeef:2020"},
		TargetUtilization:         cfgv1.DefaultTargetUtilizationPercent,
		DefaultRequestsMultiplier: cfgv1.DefaultRequestsMultiplier,
	}
	targetLoadPackingConfig := config.PluginConfig{
		Name: Name,
		Args: &targetLoadPackingArgs,
	}
	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nil)
	fh, err := testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{targetLoadPackingConfig},
		"kube-scheduler", runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.Nil(t, err)
	p, err := New(ctx, &targetLoadPackingArgs, fh)
	assert.NotNil(t, p)
	assert.Nil(t, err)
}

func TestTargetLoadPackingScoring(t *testing.T) {

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	targetLoadPackingArgs := pluginConfig.TargetLoadPackingArgs{
		TrimaranSpec:              pluginConfig.TrimaranSpec{WatcherAddress: "http://deadbeef:2020"},
		TargetUtilization:         cfgv1.DefaultTargetUtilizationPercent,
		DefaultRequestsMultiplier: cfgv1.DefaultRequestsMultiplier,
	}
	targetLoadPackingConfig := config.PluginConfig{
		Name: Name,
		Args: &targetLoadPackingArgs,
	}

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
									Value:    0,
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: cfgv1.DefaultTargetUtilizationPercent},
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
									Value:    float64(cfgv1.DefaultTargetUtilizationPercent + 10),
									Operator: watcher.Latest,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 33},
			},
		},
		{
			test: "excess utilization returns min score",
			pod:  getPodWithContainersAndOverhead(0, 1000),
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
									Value:    30,
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
			fh, err := testutil.NewFramework(ctx, registeredPlugins, []config.PluginConfig{targetLoadPackingConfig},
				"default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			targetLoadPackingArgs := pluginConfig.TargetLoadPackingArgs{
				TrimaranSpec:              pluginConfig.TrimaranSpec{WatcherAddress: server.URL},
				TargetUtilization:         cfgv1.DefaultTargetUtilizationPercent,
				DefaultRequestsMultiplier: cfgv1.DefaultRequestsMultiplier,
			}
			p, _ := New(ctx, &targetLoadPackingArgs, fh)
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
}

func BenchmarkTargetLoadPackingPlugin(b *testing.B) {
	tests := []struct {
		name     string
		podsNum  int64
		nodesNum int64
	}{
		{
			name:     "100nodes",
			podsNum:  1000,
			nodesNum: 100,
		},
		{
			name:     "1000nodes",
			podsNum:  10000,
			nodesNum: 1000,
		},
		{
			name:     "5000nodes",
			podsNum:  30000,
			nodesNum: 5000,
		},
	}

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterScorePlugin(Name, New, 1),
	}

	bfbpArgs := pluginConfig.TargetLoadPackingArgs{}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			pod := st.MakePod().Name("p").Label("foo", "").Obj()
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			nodes := getNodes(tt.nodesNum)
			snapshot := newTestSharedLister(nil, nodes)

			nodeMetricsMap := make(map[string]watcher.NodeMetrics)
			nodeMetrics := watcher.NodeMetrics{
				Metrics: []watcher.Metric{
					{
						Type:     watcher.CPU,
						Value:    0,
						Operator: watcher.Latest,
					},
				},
			}
			for _, node := range nodes {
				nodeMetricsMap[node.Name] = nodeMetrics
			}
			watcherResponse := watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: nodeMetricsMap,
				},
			}

			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(watcherResponse)
				if err != nil {
					klog.ErrorS(err, "Error marshalling watcher response")
				}
				resp.Write(bytes)
			}))

			bfbpArgs.WatcherAddress = server.URL
			defer server.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fh, err := tf.NewFramework(ctx, registeredPlugins, "default-scheduler", runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(b, err)
			pl, err := New(ctx, &bfbpArgs, fh)
			assert.Nil(b, err)
			scorePlugin := pl.(framework.ScorePlugin)
			informerFactory.Start(context.Background().Done())
			informerFactory.WaitForCacheSync(context.Background().Done())

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				gotList := make(framework.NodeScoreList, len(nodes))
				scoreNode := func(i int) {
					n := nodes[i]
					score, _ := scorePlugin.Score(ctx, state, pod, n.Name)
					gotList[i] = framework.NodeScore{Name: n.Name, Score: score}
				}
				Until(ctx, len(nodes), scoreNode)
				status := (scorePlugin.(framework.ScoreExtensions)).NormalizeScore(ctx, state, pod, gotList)
				assert.True(b, status.IsSuccess())
			}
		})
	}
}

const parallelism = 16

// Copied from k8s internal package
// chunkSizeFor returns a chunk size for the given number of items to use for
// parallel work. The size aims to produce good CPU utilization.
func chunkSizeFor(n int) workqueue.Options {
	s := int(math.Sqrt(float64(n)))
	if r := n/parallelism + 1; s > r {
		s = r
	} else if s < 1 {
		s = 1
	}
	return workqueue.WithChunkSize(s)
}

// Copied from k8s internal package
// Until is a wrapper around workqueue.ParallelizeUntil to use in scheduling algorithms.
func Until(ctx context.Context, pieces int, doWorkPiece workqueue.DoWorkPieceFunc) {
	workqueue.ParallelizeUntil(ctx, parallelism, pieces, doWorkPiece, chunkSizeFor(pieces))
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

func getPodWithContainersAndOverhead(overhead int64, requests ...int64) *v1.Pod {
	newPod := st.MakePod()
	newPod.Spec.Overhead = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.Overhead[v1.ResourceCPU] = *resource.NewMilliQuantity(overhead, resource.DecimalSI)

	for i := 0; i < len(requests); i++ {
		newPod.Container("test-container-" + strconv.Itoa(i))
	}
	for i, request := range requests {
		newPod.Spec.Containers[i].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
	}
	return newPod.Obj()
}

func getNodes(nodesNum int64) (nodes []*v1.Node) {
	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "64000m",
		v1.ResourceMemory: "346Gi",
	}
	var i int64
	for i = 0; i < nodesNum; i++ {
		nodes = append(nodes, st.MakeNode().Name(fmt.Sprintf("node-%v", i)).Capacity(nodeResources).Obj())
	}
	return
}
