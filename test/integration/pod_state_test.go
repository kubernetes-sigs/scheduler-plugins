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
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/podstate"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPodStatePlugin(t *testing.T) {
	testCtx := &testContext{}

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	pause := imageutils.GetPauseImageName()
	nodeNominatedSelector := map[string]string{"nominated": "true"}
	tests := []struct {
		name         string
		pod          *v1.Pod
		pods         []*v1.Pod
		nodes        []*v1.Node
		expectedNode string
	}{
		{
			name: "pod will be scheduled to nodes has more terminating pods",
			pod:  st.MakePod().Name("p1").UID("p1").Container(pause).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod-1").UID("pod-1").Node("node-a").Terminating().Container(pause).Obj(),
				st.MakePod().Name("pod-2").UID("pod-2").Node("node-a").Terminating().Container(pause).Obj(),
				st.MakePod().Name("pod-3").UID("pod-3").Node("node-b").Container(pause).Obj(),
				st.MakePod().Name("pod-4").UID("pod-4").Node("node-b").Container(pause).Obj(),
				st.MakePod().Name("pod-5").UID("pod-5").NodeSelector(nodeNominatedSelector).Node("node-c").Container(pause).Obj(),
				st.MakePod().Name("pod-6").UID("pod-6").NodeSelector(nodeNominatedSelector).Priority(highPriority).Container(pause).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-c").Label("node", "node-c").Label("nominated", "true").Obj(),
			},
			expectedNode: "node-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

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
					{Name: podstate.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			}

			ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
			createNamespace(t, testCtx, ns)

			testCtx = initTestSchedulerWithOptions(
				t,
				testCtx,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{podstate.Name: podstate.New}),
			)
			syncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			defer cleanupTest(t, testCtx)

			// Create nodes and pods.
			for _, node := range tt.nodes {
				if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}

			// Create existing Pods on node.
			var pods []*v1.Pod
			for _, pod := range tt.pods {
				// Create Nominated Pods by setting two pods exposing same host port in one node.
				if _, ok := pod.Spec.NodeSelector["nominated"]; ok {
					pod.Spec.Containers[0].Ports = []v1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}}
				}
				p, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create existing Pod %q: %v", pod.Name, err)
				}
				pods = append(pods, p)

				// Ensure the existing Pods are scheduled successfully except for the nominated pods.
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
					return podScheduled(cs, ns, pod.Name), nil
				}); err != nil {
					t.Logf("pod %q failed to be scheduled", pod.Name)
				}

				// Create Terminating Pods by deleting pods from cluster.
				if pod.DeletionTimestamp != nil {
					if err := cs.CoreV1().Pods(ns).Delete(testCtx.Ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
						t.Fatalf("failed to delete existing Pod %q: %v", pod.Name, err)
					}
				}
			}
			defer cleanupPods(t, testCtx, pods)

			// Create Pod to be scheduled.
			p, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, tt.pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create Pod %q: %v", tt.pod.Name, err)
			}
			defer cleanupPods(t, testCtx, []*v1.Pod{p})

			// Ensure the Pod is scheduled successfully.
			if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 60*time.Second, false, func(ctx context.Context) (bool, error) {
				return podScheduled(cs, ns, tt.pod.Name), nil
			}); err != nil {
				t.Errorf("pod %q failed to be scheduled: %v", tt.pod.Name, err)
			}

			// Lastly, verify pod gets scheduled to the expected node.
			pod, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, tt.pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get Pod %q: %v", tt.pod.Name, err)
			}
			if pod.Spec.NodeName != tt.expectedNode {
				t.Errorf("Pod %q is expected on node %q, but found on node %q",
					pod.Name, tt.expectedNode, pod.Spec.NodeName)
			}
		})
	}
}
