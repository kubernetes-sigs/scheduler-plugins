/*
Copyright 2024 The Kubernetes Authors.

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
	"sigs.k8s.io/scheduler-plugins/pkg/nodemetadata"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestNodeMetadataPlugin(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*v1.Pod
		nodes         []*v1.Node
		pluginConfig  *schedconfig.NodeMetadataArgs
		expectedNodes map[string]sets.Set[string] // pod name to expected node name mapping
	}{
		{
			name: "highest numeric label - pods should prefer nodes with higher priority values",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-3").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-low").Label("priority", "10").Obj(),
				st.MakeNode().Name("node-medium").Label("priority", "50").Obj(),
				st.MakeNode().Name("node-high").Label("priority", "100").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  schedconfig.MetadataSourceLabel,
				MetadataType:    schedconfig.MetadataTypeNumber,
				ScoringStrategy: schedconfig.ScoringStrategyHighest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-high"),
				"pod-2": sets.New("node-high"),
				"pod-3": sets.New("node-high"),
			},
		},
		{
			name: "lowest numeric annotation - pods should prefer nodes with lower cost values",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-3").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-expensive").Annotation("cost", "100").Obj(),
				st.MakeNode().Name("node-moderate").Annotation("cost", "50").Obj(),
				st.MakeNode().Name("node-cheap").Annotation("cost", "10").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "cost",
				MetadataSource:  schedconfig.MetadataSourceAnnotation,
				MetadataType:    schedconfig.MetadataTypeNumber,
				ScoringStrategy: schedconfig.ScoringStrategyLowest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-cheap"),
				"pod-2": sets.New("node-cheap"),
				"pod-3": sets.New("node-cheap"),
			},
		},
		{
			name: "newest timestamp annotation - pods should prefer nodes with newest timestamps",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-old").Annotation("lastUpdate", "2024-01-01T00:00:00Z").Obj(),
				st.MakeNode().Name("node-recent").Annotation("lastUpdate", "2024-06-01T00:00:00Z").Obj(),
				st.MakeNode().Name("node-newest").Annotation("lastUpdate", "2024-12-01T00:00:00Z").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "lastUpdate",
				MetadataSource:  schedconfig.MetadataSourceAnnotation,
				MetadataType:    schedconfig.MetadataTypeTimestamp,
				ScoringStrategy: schedconfig.ScoringStrategyNewest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-newest"),
				"pod-2": sets.New("node-newest"),
			},
		},
		{
			name: "oldest timestamp annotation - pods should prefer nodes with oldest timestamps",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-old").Annotation("provisionedAt", "2024-01-01T00:00:00Z").Obj(),
				st.MakeNode().Name("node-recent").Annotation("provisionedAt", "2024-06-01T00:00:00Z").Obj(),
				st.MakeNode().Name("node-newest").Annotation("provisionedAt", "2024-12-01T00:00:00Z").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "provisionedAt",
				MetadataSource:  schedconfig.MetadataSourceAnnotation,
				MetadataType:    schedconfig.MetadataTypeTimestamp,
				ScoringStrategy: schedconfig.ScoringStrategyOldest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-old"),
				"pod-2": sets.New("node-old"),
			},
		},
		{
			name: "mixed nodes - some with metadata, some without - pods should prefer nodes with metadata",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-3").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-no-metadata-1").Obj(),
				st.MakeNode().Name("node-with-metadata").Label("tier", "100").Obj(),
				st.MakeNode().Name("node-no-metadata-2").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "tier",
				MetadataSource:  schedconfig.MetadataSourceLabel,
				MetadataType:    schedconfig.MetadataTypeNumber,
				ScoringStrategy: schedconfig.ScoringStrategyHighest,
			},
			expectedNodes: map[string]sets.Set[string]{
				// All pods should prefer the node with metadata (gets MinNodeScore)
				// Nodes without metadata get 0 score, which is less than MinNodeScore
				"pod-1": sets.New("node-with-metadata"),
				"pod-2": sets.New("node-with-metadata"),
				"pod-3": sets.New("node-with-metadata"),
			},
		},
		{
			name: "decimal numbers in labels - should handle floating point values",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
				st.MakePod().Name("pod-2").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-low-score").Label("performance", "2.5").Obj(),
				st.MakeNode().Name("node-high-score").Label("performance", "9.8").Obj(),
				st.MakeNode().Name("node-medium-score").Label("performance", "5.0").Obj(),
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "performance",
				MetadataSource:  schedconfig.MetadataSourceLabel,
				MetadataType:    schedconfig.MetadataTypeNumber,
				ScoringStrategy: schedconfig.ScoringStrategyHighest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-high-score"),
				"pod-2": sets.New("node-high-score"),
			},
		},
		{
			name: "unix timestamp in annotations - should handle epoch time",
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").Container(imageutils.GetPauseImageName()).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-old-epoch").Annotation("created", "1609459200").Obj(),    // 2021-01-01
				st.MakeNode().Name("node-recent-epoch").Annotation("created", "1672531200").Obj(), // 2023-01-01
				st.MakeNode().Name("node-newest-epoch").Annotation("created", "1704067200").Obj(), // 2024-01-01
			},
			pluginConfig: &schedconfig.NodeMetadataArgs{
				MetadataKey:     "created",
				MetadataSource:  schedconfig.MetadataSourceAnnotation,
				MetadataType:    schedconfig.MetadataTypeNumber,
				ScoringStrategy: schedconfig.ScoringStrategyHighest,
			},
			expectedNodes: map[string]sets.Set[string]{
				"pod-1": sets.New("node-newest-epoch"),
			},
		},
	}

	for _, tc := range tests {
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

			// Disable default plugins and enable only NodeMetadata for scoring
			cfg.Profiles[0].Plugins.PreScore = schedapi.PluginSet{
				Disabled: []schedapi.Plugin{{Name: "*"}},
			}
			cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: nodemetadata.Name, Weight: 10},
				},
				Disabled: []schedapi.Plugin{{Name: "*"}},
			}

			cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
				Name: nodemetadata.Name,
				Args: tc.pluginConfig,
			})

			ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
			createNamespace(t, testCtx, ns)

			testCtx = initTestSchedulerWithOptions(
				t,
				testCtx,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{nodemetadata.Name: nodemetadata.New}),
			)
			syncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			defer cleanupTest(t, testCtx)

			// Create nodes
			for _, node := range tc.nodes {
				if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create node %q: %v", node.Name, err)
				}
			}

			// Create and schedule pods
			var createdPods []*v1.Pod
			for _, pod := range tc.pods {
				pod.Namespace = ns
				p, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create pod %q: %v", pod.Name, err)
				}
				createdPods = append(createdPods, p)
			}
			defer cleanupPods(t, testCtx, createdPods)

			// Wait for all pods to be scheduled
			for _, pod := range tc.pods {
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 10*time.Second, false, func(ctx context.Context) (bool, error) {
					return podScheduled(t, cs, ns, pod.Name), nil
				}); err != nil {
					t.Fatalf("pod %q failed to be scheduled: %v", pod.Name, err)
				}
			}

			// Verify pods are scheduled on expected nodes
			for _, pod := range tc.pods {
				p, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get pod %q: %v", pod.Name, err)
				}

				expectedNodeSet, exists := tc.expectedNodes[pod.Name]
				if !exists {
					t.Fatalf("no expected node set for pod %q", pod.Name)
				}

				if !expectedNodeSet.Has(p.Spec.NodeName) {
					t.Errorf("pod %q scheduled on unexpected node %q, expected one of %v",
						pod.Name, p.Spec.NodeName, expectedNodeSet.UnsortedList())
				} else {
					t.Logf("pod %q correctly scheduled on node %q", pod.Name, p.Spec.NodeName)
				}
			}
		})
	}
}

// TestNodeMetadataPluginWithMultiplePods tests the plugin's behavior when multiple pods
// are scheduled sequentially and verifies score normalization works correctly.
func TestNodeMetadataPluginWithMultiplePods(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Profiles[0].Plugins.PreScore = schedapi.PluginSet{
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
		Enabled: []schedapi.Plugin{
			{Name: nodemetadata.Name, Weight: 10},
		},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}

	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: nodemetadata.Name,
		Args: &schedconfig.NodeMetadataArgs{
			MetadataKey:     "priority",
			MetadataSource:  schedconfig.MetadataSourceLabel,
			MetadataType:    schedconfig.MetadataTypeNumber,
			ScoringStrategy: schedconfig.ScoringStrategyHighest,
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{nodemetadata.Name: nodemetadata.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	// Create nodes with different priority values
	nodes := []*v1.Node{
		st.MakeNode().Name("node-priority-10").Label("priority", "10").Obj(),
		st.MakeNode().Name("node-priority-50").Label("priority", "50").Obj(),
		st.MakeNode().Name("node-priority-100").Label("priority", "100").Obj(),
	}
	for _, node := range nodes {
		if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create node %q: %v", node.Name, err)
		}
	}

	// Create multiple pods and verify they all prefer the highest priority node
	numPods := 5
	var createdPods []*v1.Pod
	for i := 0; i < numPods; i++ {
		podName := fmt.Sprintf("test-pod-%d", i)
		pod := st.MakePod().Namespace(ns).Name(podName).Container(imageutils.GetPauseImageName()).Obj()

		p, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create pod %q: %v", podName, err)
		}
		createdPods = append(createdPods, p)

		// Wait for pod to be scheduled
		if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 10*time.Second, false, func(ctx context.Context) (bool, error) {
			return podScheduled(t, cs, ns, podName), nil
		}); err != nil {
			t.Fatalf("pod %q failed to be scheduled: %v", podName, err)
		}

		// Verify pod is on the highest priority node
		p, err = cs.CoreV1().Pods(ns).Get(testCtx.Ctx, podName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get pod %q: %v", podName, err)
		}

		if p.Spec.NodeName != "node-priority-100" {
			t.Errorf("pod %q scheduled on node %q, expected node-priority-100", podName, p.Spec.NodeName)
		} else {
			t.Logf("pod %q correctly scheduled on node %q", podName, p.Spec.NodeName)
		}
	}
	defer cleanupPods(t, testCtx, createdPods)
}
