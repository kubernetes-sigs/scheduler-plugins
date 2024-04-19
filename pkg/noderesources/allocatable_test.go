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

package noderesources

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	schedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

func TestNodeResourcesAllocatable(t *testing.T) {
	labels1 := map[string]string{
		"foo": "bar",
		"baz": "blah",
	}
	labels2 := map[string]string{
		"bar": "foo",
		"baz": "blah",
	}

	machine1Spec := v1.PodSpec{
		NodeName: "machine1",
	}
	machine2Spec := v1.PodSpec{
		NodeName: "machine2",
	}
	noResources := v1.PodSpec{
		Containers: []v1.Container{},
	}
	scheduledPod := v1.PodSpec{
		NodeName: "machine1",
		Containers: []v1.Container{
			{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1000m"),
						v1.ResourceMemory: resource.MustParse("0"),
					},
				},
			},
			{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2000m"),
						v1.ResourceMemory: resource.MustParse("0"),
					},
				},
			},
		},
	}
	cpuAndMemory := makePod("cpuAndMemory", v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("1000m"),
		v1.ResourceMemory: resource.MustParse("1Gi")},
	)
	bigCpu := makePod("bigCpu", v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("8000m"),
		v1.ResourceMemory: resource.MustParse("1Gi")},
	)

	// 1 millicore weighted the same as 1 MiB.
	defaultResourceAllocatableSet := []schedulerconfig.ResourceSpec{
		{Name: string(v1.ResourceCPU), Weight: 1 << 20},
		{Name: string(v1.ResourceMemory), Weight: 1},
	}

	// 1 millicore weighted the same as 1 GiB.
	cpuResourceAllocatableSet := []schedulerconfig.ResourceSpec{
		{Name: string(v1.ResourceCPU), Weight: 1 << 30},
		{Name: string(v1.ResourceMemory), Weight: 1},
	}

	modeLeast := config.Least
	modeMost := config.Most
	tests := []struct {
		pod          *v1.Pod
		pods         []*v1.Pod
		nodeInfos    []*framework.NodeInfo
		args         config.NodeResourcesAllocatableArgs
		wantErr      string
		expectedList framework.NodeScoreList
		name         string
	}{
		{
			pod:          &v1.Pod{Spec: noResources},
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 10000), makeNodeInfo("machine2", 4000, 10000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "nothing scheduled, nothing requested",
		},
		{
			pod:          cpuAndMemory,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 10000), makeNodeInfo("machine2", 6000, 10000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MaxNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "nothing scheduled, resources requested, differently sized machines, least mode",
		},
		{
			pod:          cpuAndMemory,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 10000), makeNodeInfo("machine2", 6000, 10000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeMost},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MaxNodeScore}},
			name:         "nothing scheduled, resources requested, differently sized machines, most mode",
		},
		{
			pod:          &v1.Pod{Spec: noResources},
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 10000), makeNodeInfo("machine2", 4000, 10000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "no resources requested, pods scheduled",
			pods: []*v1.Pod{
				{Spec: machine1Spec, ObjectMeta: metav1.ObjectMeta{Labels: labels2}},
				{Spec: machine1Spec, ObjectMeta: metav1.ObjectMeta{Labels: labels1}},
				{Spec: machine2Spec, ObjectMeta: metav1.ObjectMeta{Labels: labels1}},
				{Spec: machine2Spec, ObjectMeta: metav1.ObjectMeta{Labels: labels1}},
			},
		},
		{
			pod:          &v1.Pod{Spec: noResources},
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 10000, 20000), makeNodeInfo("machine2", 10000, 20000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "no resources requested, pods scheduled with resources",
			pods: []*v1.Pod{
				{Spec: scheduledPod},
			},
		},
		{
			pod:          cpuAndMemory,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 10000, 20000), makeNodeInfo("machine2", 10000, 20000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "resources requested, pods scheduled with resources",
			pods: []*v1.Pod{
				{Spec: scheduledPod},
			},
		},
		{
			pod:          bigCpu,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 1000), makeNodeInfo("machine2", 5000, 1000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MaxNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "resources requested with more than the node, differently sized machines, least mode",
		},
		{
			pod:          bigCpu,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 4000, 1000), makeNodeInfo("machine2", 5000, 1000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeMost},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MaxNodeScore}},
			name:         "resources requested with more than the node, differently sized machines, most mode",
		},
		{
			pod:          cpuAndMemory,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 1000, 2000), makeNodeInfo("machine2", 1005, 1000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: cpuResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MaxNodeScore}, {Name: "machine2", Score: framework.MinNodeScore}},
			name:         "nothing scheduled, resources requested, differently sized machines, cpu weighted, most mode",
		},
		{
			pod:          cpuAndMemory,
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("machine1", 1000, 2000), makeNodeInfo("machine2", 1005, 1000)},
			args:         config.NodeResourcesAllocatableArgs{Resources: cpuResourceAllocatableSet, Mode: modeMost},
			expectedList: []framework.NodeScore{{Name: "machine1", Score: framework.MinNodeScore}, {Name: "machine2", Score: framework.MaxNodeScore}},
			name:         "nothing scheduled, resources requested, differently sized machines, cpu weighted, least mode",
		},
		{
			pod: cpuAndMemory,
			nodeInfos: []*framework.NodeInfo{
				makeNodeInfo("machine1", 1000, 1000*1<<20),
				makeNodeInfo("machine2", 2000, 2000*1<<20),
				makeNodeInfo("machine3", 3000, 3000*1<<20)},
			args: config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeLeast},
			expectedList: []framework.NodeScore{
				{Name: "machine1", Score: framework.MaxNodeScore},
				{Name: "machine2", Score: (framework.MinNodeScore + framework.MaxNodeScore) / 2},
				{Name: "machine3", Score: framework.MinNodeScore}},
			name: "nothing scheduled, resources requested, 3 differently sized machines, least mode",
		},
		{
			pod: cpuAndMemory,
			nodeInfos: []*framework.NodeInfo{
				makeNodeInfo("machine1", 1000, 1000*1<<20),
				makeNodeInfo("machine2", 2000, 2000*1<<20),
				makeNodeInfo("machine3", 3000, 3000*1<<20)},
			args: config.NodeResourcesAllocatableArgs{Resources: defaultResourceAllocatableSet, Mode: modeMost},
			expectedList: []framework.NodeScore{
				{Name: "machine1", Score: framework.MinNodeScore},
				{Name: "machine2", Score: (framework.MinNodeScore + framework.MaxNodeScore) / 2},
				{Name: "machine3", Score: framework.MaxNodeScore}},
			name: "nothing scheduled, resources requested, 3 differently sized machines, most mode",
		},
		{
			// resource with negative weight is not allowed
			pod:       cpuAndMemory,
			nodeInfos: []*framework.NodeInfo{makeNodeInfo("machine", 4000, 10000)},
			args:      config.NodeResourcesAllocatableArgs{Resources: []schedulerconfig.ResourceSpec{{Name: "memory", Weight: -1}, {Name: "cpu", Weight: 1}}},
			wantErr:   "resource Weight of memory should be a positive value, got -1",
			name:      "resource with negative weight",
		},
		{
			// resource with zero weight is not allowed
			pod:       cpuAndMemory,
			nodeInfos: []*framework.NodeInfo{makeNodeInfo("machine", 4000, 10000)},
			args:      config.NodeResourcesAllocatableArgs{Resources: []schedulerconfig.ResourceSpec{{Name: "memory", Weight: 1}, {Name: "cpu", Weight: 0}}},
			wantErr:   "resource Weight of cpu should be a positive value, got 0",
			name:      "resource with zero weight",
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
				tf.RegisterScorePlugin(AllocatableName, NewAllocatable, 1),
			}
			fakeSharedLister := &fakeSharedLister{nodes: test.nodeInfos}

			fh, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithSnapshotSharedLister(fakeSharedLister),
			)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			alloc, err := NewAllocatable(ctx, &test.args, fh)

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

			var gotList framework.NodeScoreList
			plugin := alloc.(framework.ScorePlugin)
			for i := range test.nodeInfos {
				score, err := plugin.Score(context.Background(), nil, test.pod, test.nodeInfos[i].Node().Name)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				gotList = append(gotList, framework.NodeScore{Name: test.nodeInfos[i].Node().Name, Score: score})
			}

			status := plugin.ScoreExtensions().NormalizeScore(context.Background(), nil, test.pod, gotList)
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}

			for i := range gotList {
				if !reflect.DeepEqual(test.expectedList[i].Score, gotList[i].Score) {
					t.Errorf("expected %#v, got %#v", test.expectedList[i].Score, gotList[i].Score)
				}
			}
		})
	}
}

func makeNodeInfo(node string, milliCPU, memory int64) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(milliCPU, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(milliCPU, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
			},
		},
	})
	return ni
}

func makePod(name string, requests v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: name,
					Resources: v1.ResourceRequirements{
						Requests: requests,
					},
				},
			},
		},
	}
}

var _ framework.SharedLister = &fakeSharedLister{}

type fakeSharedLister struct {
	nodes []*framework.NodeInfo
}

func (f *fakeSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *fakeSharedLister) NodeInfos() framework.NodeInfoLister {
	return tf.NodeInfoLister(f.nodes)
}
