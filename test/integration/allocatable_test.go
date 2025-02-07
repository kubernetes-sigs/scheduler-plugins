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

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	schedconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesources"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestAllocatablePlugin(t *testing.T) {

	smallNodeCapacity := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}
	bigNodeCapacity := map[v1.ResourceName]string{
		v1.ResourcePods:   "32",
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "5000",
	}
	smallPodReq := map[v1.ResourceName]string{
		v1.ResourceMemory: "100",
	}
	bigPodReq := map[v1.ResourceName]string{
		v1.ResourceMemory: "2000",
	}

	testCases := []struct {
		name          string
		pods          []*v1.Pod
		nodes         []*v1.Node
		modeType      schedconfig.ModeType
		expectedNodes map[string]sets.Set[string] // pod name to expected node name mapping
	}{
		{
			name: "least modeType the small pods should land on the small nodes and the big pod should land on the big node",
			pods: []*v1.Pod{
				st.MakePod().Name("small-1").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-2").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-3").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-4").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("big-1").Container(imageutils.GetPauseImageName()).Req(bigPodReq).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("fake-node-small-1").Label("node", "fake-node-small-1").Capacity(smallNodeCapacity).Obj(),
				st.MakeNode().Name("fake-node-small-2").Label("node", "fake-node-small-2").Capacity(smallNodeCapacity).Obj(),
				st.MakeNode().Name("fake-node-big").Label("node", "fake-node-big").Capacity(bigNodeCapacity).Obj(),
			},
			expectedNodes: map[string]sets.Set[string]{
				"small-1": sets.New("fake-node-small-1", "fake-node-small-2"),
				"small-2": sets.New("fake-node-small-1", "fake-node-small-2"),
				"small-3": sets.New("fake-node-small-1", "fake-node-small-2"),
				"small-4": sets.New("fake-node-small-1", "fake-node-small-2"),
				"big-1":   sets.New("fake-node-big"),
			},
			modeType: schedconfig.Least,
		},
		{
			name: "most modeType the small pods should land on the big node and the big pod should also land on the big node",
			pods: []*v1.Pod{
				st.MakePod().Name("small-1").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-2").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-3").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("small-4").Container(imageutils.GetPauseImageName()).Req(smallPodReq).Obj(),
				st.MakePod().Name("big-1").Container(imageutils.GetPauseImageName()).Req(bigPodReq).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("fake-node-small-1").Label("node", "fake-node-small-1").Capacity(smallNodeCapacity).Obj(),
				st.MakeNode().Name("fake-node-small-2").Label("node", "fake-node-small-2").Capacity(smallNodeCapacity).Obj(),
				st.MakeNode().Name("fake-node-big").Label("node", "fake-node-big").Capacity(bigNodeCapacity).Obj(),
			},
			expectedNodes: map[string]sets.Set[string]{
				"small-1": sets.New("fake-node-big"),
				"small-2": sets.New("fake-node-big"),
				"small-3": sets.New("fake-node-big"),
				"small-4": sets.New("fake-node-big"),
				"big-1":   sets.New("fake-node-big"),
			},
			modeType: schedconfig.Most,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testCtx := &testContext{}
			testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

			cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
			testCtx.ClientSet = cs
			testCtx.KubeConfig = globalKubeConfig

			cfg, err := util.NewDefaultSchedulerComponentConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Profiles[0].Plugins.PreScore = schedapi.PluginSet{Disabled: []schedapi.Plugin{{Name: "*"}}}
			cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
				Enabled:  []schedapi.Plugin{{Name: noderesources.AllocatableName, Weight: 50000}},
				Disabled: []schedapi.Plugin{{Name: "*"}},
			}

			cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
				Name: noderesources.AllocatableName,
				Args: &schedconfig.NodeResourcesAllocatableArgs{
					Mode: tc.modeType,
					Resources: []schedapi.ResourceSpec{
						{Name: string(v1.ResourceMemory), Weight: 10},
					},
				},
			})

			ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
			createNamespace(t, testCtx, ns)

			testCtx = initTestSchedulerWithOptions(
				t,
				testCtx,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{noderesources.AllocatableName: noderesources.NewAllocatable}),
			)
			syncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			defer cleanupTest(t, testCtx)

			// Create nodes.
			for _, node := range tc.nodes {
				_, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Node %q: %v", node.Name, err)
				}
			}

			// Create the Pods.
			for _, pod := range tc.pods {
				// set namespace to pods
				pod.SetNamespace(ns)
				_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}
			defer cleanupPods(t, testCtx, tc.pods)

			for _, pod := range tc.pods {
				err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 60*time.Second, false, func(ctx context.Context) (bool, error) {
					return podScheduled(t, cs, pod.Namespace, pod.Name), nil
				})
				if err != nil {
					t.Fatalf("Waiting for pod %q to be scheduled, error: %v", pod.Name, err.Error())
				}

				scheduledPod, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}

				expectedNodes, exists := tc.expectedNodes[pod.Name]
				if !exists || !expectedNodes.Has(scheduledPod.Spec.NodeName) {
					t.Errorf("Pod %q is expected on node %q, but found on node %q", pod.Name, expectedNodes.UnsortedList(), scheduledPod.Spec.NodeName)
				} else {
					t.Logf("Pod %q is on node %q as expected.", pod.Name, scheduledPod.Spec.NodeName)
				}
			}
		})
	}
}
