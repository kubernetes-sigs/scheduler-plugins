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

package podstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"

	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPodState(t *testing.T) {
	tests := []struct {
		nodeInfos    []*framework.NodeInfo
		wantErr      string
		expectedList framework.NodeScoreList
		name         string
	}{
		{
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("node1", 6, 0, 10), makeNodeInfo("node2", 3, 0, 10), makeNodeInfo("node3", 0, 0, 10)},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 50}, {Name: "node3", Score: framework.MinNodeScore}},
			name:         "node has more terminating pods will be scored with higher score, node has regular pods only will be scored with the lowest score.",
		},
		{
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("node1", 0, 2, 10), makeNodeInfo("node2", 0, 1, 10), makeNodeInfo("node3", 0, 0, 10)},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MinNodeScore}, {Name: "node2", Score: 50}, {Name: "node3", Score: framework.MaxNodeScore}},
			name:         "node has more nominated pods will be scored with lower score, node has regular pods only will be scored with the highest score.",
		},
		{
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("node1", 5, 2, 10), makeNodeInfo("node2", 3, 1, 10)},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: framework.MinNodeScore}},
			name:         "node has more (terminatingPodNumber - nominatedPodNumber) will be scored with higher score",
		},
		{
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("node1", 5, 4, 10), makeNodeInfo("node2", 3, 1, 10)},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MinNodeScore}, {Name: "node2", Score: framework.MaxNodeScore}},
			name:         "node has less (terminatingPodNumber - nominatedPodNumber) will be scored with lower score",
		},
		{
			nodeInfos:    []*framework.NodeInfo{makeNodeInfo("node1", 5, 0, 10), makeNodeInfo("node2", 3, 1, 10), makeNodeInfo("node3", 2, 1, 10), makeNodeInfo("node4", 0, 1, 10)},
			expectedList: []framework.NodeScore{{Name: "node1", Score: framework.MaxNodeScore}, {Name: "node2", Score: 50}, {Name: "node3", Score: 33}, {Name: "node4", Score: framework.MinNodeScore}},
			name:         "node has more (terminatingPodNumber - nominatedPodNumber) will be scored with higher score",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)

			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterScorePlugin(Name, New, 1),
			}
			fakeSharedLister := &fakeSharedLister{nodes: test.nodeInfos}

			fh, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithSnapshotSharedLister(fakeSharedLister),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
			)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}
			// initialize nominated pod by adding nominated pods into nominatedPodMap
			for _, n := range test.nodeInfos {
				for _, pi := range n.Pods {
					if pi.Pod.Status.NominatedNodeName != "" {
						addNominatedPod(logger, pi, n.Node().Name, fh)
					}
				}
			}
			pe, _ := New(nil, nil, fh)
			var gotList framework.NodeScoreList
			plugin := pe.(framework.ScorePlugin)
			for i, n := range test.nodeInfos {
				score, err := plugin.Score(context.Background(), nil, nil, n.Node().Name)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				gotList = append(gotList, framework.NodeScore{Name: test.nodeInfos[i].Node().Name, Score: score})
			}

			status := plugin.ScoreExtensions().NormalizeScore(context.Background(), nil, nil, gotList)
			if !status.IsSuccess() {
				t.Errorf("unexpected error: %v", status)
			}

			for i := range gotList {
				if test.expectedList[i].Score != gotList[i].Score {
					t.Errorf("expected %#v, got %#v", test.expectedList[i].Score, gotList[i].Score)
				}
			}
		})
	}
}

func makeNodeInfo(node string, terminatingPodNumber, nominatedPodNumber, regularPodNumber int) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	for i := 0; i < terminatingPodNumber; i++ {
		podInfo := &framework.PodInfo{
			Pod: makeTerminatingPod(fmt.Sprintf("tpod_%s_%v", node, i+1)),
		}
		ni.Pods = append(ni.Pods, podInfo)
	}
	for i := 0; i < nominatedPodNumber; i++ {
		podInfo := &framework.PodInfo{
			Pod: makeNominatedPod(fmt.Sprintf("npod_%s_%v", node, i+1), node),
		}
		ni.Pods = append(ni.Pods, podInfo)
	}
	for i := 0; i < regularPodNumber; i++ {
		podInfo := &framework.PodInfo{
			Pod: makeRegularPod(fmt.Sprintf("rpod_%s_%v", node, i+1)),
		}
		ni.Pods = append(ni.Pods, podInfo)
	}
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
	})
	return ni
}

func makeTerminatingPod(name string) *v1.Pod {
	deletionTimestamp := metav1.Time{Time: time.Now()}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			DeletionTimestamp: &deletionTimestamp,
		},
	}
}

func makeNominatedPod(podName string, nodeName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			UID:  types.UID(podName),
		},
		Status: v1.PodStatus{
			NominatedNodeName: nodeName,
		},
	}
}

func makeRegularPod(name string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func addNominatedPod(logger klog.Logger, pi *framework.PodInfo, nodeName string, fh framework.Handle) *framework.PodInfo {
	fh.AddNominatedPod(logger, pi, &framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: nodeName})
	return pi
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
