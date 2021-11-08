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
	"testing"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/scheduler-plugins/pkg/podstate"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPodStatePlugin(t *testing.T) {
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
			cfg, err := util.NewDefaultSchedulerComponentConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: podstate.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			}

			testCtx := util.InitTestSchedulerWithOptions(
				t,
				testutils.InitTestAPIServer(t, "sched-podstate", nil),
				true,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{podstate.Name: podstate.New}),
			)
			defer testutils.CleanupTest(t, testCtx)

			cs, ns := testCtx.ClientSet, testCtx.NS.Name

			// Create nodes and pods.
			for _, node := range tt.nodes {
				if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}

			// Create existing Pods on node.
			for _, pod := range tt.pods {

				// Create Nominated Pods by setting two pods exposing same host port in one node.
				if _, ok := pod.Spec.NodeSelector["nominated"]; ok {
					pod.Spec.Containers[0].Ports = []v1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}}
				}
				if _, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create existing Pod %q: %v", pod.Name, err)
				}

				// Ensure the existing Pods are scheduled successfully except for the nominated pods.
				if err := wait.Poll(1*time.Second, 20*time.Second, func() (bool, error) {
					return podScheduled(cs, ns, pod.Name), nil
				}); err != nil {
					t.Logf("pod %q failed to be scheduled", pod.Name)
				}

				// Create Terminating Pods by deleting pods from cluster.
				if pod.DeletionTimestamp != nil {
					if err := cs.CoreV1().Pods(ns).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
						t.Fatalf("failed to delete existing Pod %q: %v", pod.Name, err)
					}
				}
			}

			// Create Pod to be scheduled.
			if _, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, tt.pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create Pod %q: %v", tt.pod.Name, err)
			}

			// Ensure the Pod is scheduled successfully.
			if err := wait.Poll(1*time.Second, 60*time.Second, func() (bool, error) {
				return podScheduled(cs, ns, tt.pod.Name), nil
			}); err != nil {
				t.Errorf("pod %q failed to be scheduled: %v", tt.pod.Name, err)
			}

			// Lastly, verify pod gets scheduled to the expected node.
			pod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), tt.pod.Name, metav1.GetOptions{})
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
