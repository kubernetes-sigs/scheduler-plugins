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

/*
import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutil "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/scheduler-plugins/pkg/crossnodepreemption"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestCrossNodePreemptionPlugin(t *testing.T) {
	fooSelector := st.MakeLabelSelector().Exists("foo").Obj()
	zeroPodRes := map[v1.ResourceName]string{v1.ResourcePods: "0"}
	pause := imageutils.GetPauseImageName()

	tests := []struct {
		name  string
		pod   *v1.Pod
		pods  []*v1.Pod
		nodes []*v1.Node
	}{
		{
			name: "PodTopologySpread: preempt 2 pods in zone1",
			pod: st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).Container(pause).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, fooSelector).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").ZeroTerminationGracePeriod().Container(pause).Obj(),
				st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").ZeroTerminationGracePeriod().Container(pause).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label("node", "node-x").Capacity(zeroPodRes).Obj(),
			},
		},
		{
			name: "PodAntiAffinity: preempt 2 pods in zone1",
			pod: st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).Container(pause).
				PodAntiAffinityExists("foo", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").ZeroTerminationGracePeriod().Container(pause).Obj(),
				st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").ZeroTerminationGracePeriod().Container(pause).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label("node", "node-x").Capacity(zeroPodRes).Obj(),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := util.NewDefaultSchedulerComponentConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Profiles[0].Plugins.PostFilter.Enabled = append(cfg.Profiles[0].Plugins.PostFilter.Enabled, schedapi.Plugin{Name: crossnodepreemption.Name})

			testCtx := testutil.InitTestSchedulerWithOptions(
				t,
				testutil.InitTestAPIServer(t, "sched-crossnodepreemption", nil),
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{crossnodepreemption.Name: crossnodepreemption.New}),
			)
			testutil.SyncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			defer testutil.CleanupTest(t, testCtx)

			cs, ns := testCtx.ClientSet, testCtx.NS.Name
			// Create nodes and pods.
			for _, node := range tt.nodes {
				if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}
			for _, pod := range tt.pods {
				if _, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create Pod %q: %v", pod.Name, err)
				}
			}

			// Create the preemptor Pod.
			if _, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, tt.pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create preemptor Pod %q: %v", tt.pod.Name, err)
			}
			// Ensure the preemtor Pod is scheduled successfully.
			if err := wait.Poll(1*time.Second, 60*time.Second, func() (bool, error) {
				return podScheduled(cs, ns, tt.pod.Name), nil
			}); err != nil {
				t.Errorf("preemptor pod %q failed to be scheduled: %v", tt.pod.Name, err)
			}

			// Lastly, existing Pods are expected to be preempted.
			for _, pod := range tt.pods {
				if err := wait.Poll(1*time.Second, 30*time.Second, func() (bool, error) {
					return podNotExist(cs, ns, pod.Name), nil
				}); err != nil {
					t.Errorf("preemptor pod %q failed to be scheduled: %v", tt.pod.Name, err)
				}
			}
		})
	}
}
*/
