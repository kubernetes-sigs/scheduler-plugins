/*
Copyright 2022 The Koordinator Authors.

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

package scarceresourceavoidance

import (
	"context"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

func TestScarceResourceAvoidance(t *testing.T) {

	scheduledPod := v1.PodSpec{
		Containers: []v1.Container{
			{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("16"),
						v1.ResourceMemory: resource.MustParse("32Gi"),
					},
				},
			},
		},
	}

	tests := []struct {
		pod          *v1.Pod
		pods         []*v1.Pod
		nodeInfos    []*framework.NodeInfo
		args         config.ScarceResourceAvoidanceArgs
		wantErr      string
		expectedList framework.NodeScoreList
		name         string
	}{
		{
			pod:       &v1.Pod{Spec: scheduledPod},
			nodeInfos: []*framework.NodeInfo{makeGPUNodeInfo("machine1", "96", "512Gi", "2"), makeNotGPUNodeInfo("machine2", "96", "512Gi")},
			args: config.ScarceResourceAvoidanceArgs{
				Resources: []v1.ResourceName{
					"nvidia.com/gpu",
				},
			},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "nothing scheduled, nothing requested",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			for _, p := range test.pods {
				podInformer.GetStore().Add(p)
			}
			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			snapshot := newTestSharedLister(nil, test.nodeInfos)

			fh, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithSnapshotSharedLister(snapshot),
			)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			alloc, err := New(ctx, &test.args, fh)

			if len(test.wantErr) != 0 {
				if err != nil && test.wantErr != err.Error() {
					t.Fatalf("got err %v, want %v", err.Error(), test.wantErr)
				} else if err == nil {
					t.Fatalf("no error produced, wanted %v", test.wantErr)
				}
				return
			}

			if err != nil && len(test.wantErr) == 0 {
				t.Fatalf("failed to initialize plugin NodeResourcesAllocatable, got error: %v", err)
			}
			cycleState := framework.NewCycleState()

			nodeScore := map[string]int64{}
			plugin := alloc.(*Plugin)
			for i := range test.nodeInfos {
				score, err1 := plugin.Score(context.Background(), cycleState, test.pod, test.nodeInfos[i].Node().Name)
				if err1 != nil {
					t.Errorf("unexpected error: %v", err1)
				}
				nodeScore[test.nodeInfos[i].Node().Name] = score
			}
			if nodeScore["machine1"] >= nodeScore["machine2"] {
				t.Fatalf("TestNodeResourceFitplus err")
			}
		})
	}
}

func makeGPUNodeInfo(node string, cpu, memory, gpu string) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(cpu),
				v1.ResourceMemory: resource.MustParse(memory),
				"nvidia.com/gpu":  resource.MustParse(gpu),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(cpu),
				v1.ResourceMemory: resource.MustParse(memory),
				"nvidia.com/gpu":  resource.MustParse(gpu),
			},
		},
	})
	return ni
}

func makeNotGPUNodeInfo(node string, cpu, memory string) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(cpu),
				v1.ResourceMemory: resource.MustParse(memory),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(cpu),
				v1.ResourceMemory: resource.MustParse(memory),
			},
		},
	})
	return ni
}

var _ framework.SharedLister = &testSharedLister{}

type testSharedLister struct {
	nodes       []*v1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func newTestSharedLister(pods []*v1.Pod, nodes []*framework.NodeInfo) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	nodeInfos := make([]*framework.NodeInfo, 0)
	nodess := make([]*v1.Node, 0)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		nodess = append(nodess, node.Node())
		if _, ok := nodeInfoMap[node.Node().Name]; !ok {
			nodeInfoMap[node.Node().Name] = framework.NewNodeInfo()
		}
		nodeInfoMap[node.Node().Name].SetNode(node.Node())
	}

	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
	}

	return &testSharedLister{
		nodes:       nodess,
		nodeInfos:   nodeInfos,
		nodeInfoMap: nodeInfoMap,
	}
}

func (f *testSharedLister) StorageInfos() framework.StorageInfoLister {
	return f
}

func (f *testSharedLister) IsPVCUsedByPods(key string) bool {
	return false
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
