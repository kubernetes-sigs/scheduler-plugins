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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesources"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestAllocatablePlugin(t *testing.T) {
	registry := fwkruntime.Registry{noderesources.AllocatableName: noderesources.NewAllocatable}
	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			Score: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: noderesources.AllocatableName,
						Weight: 50000},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			},
		},
	}

	testCtx := util.InitTestSchedulerWithOptions(
		t,
		testutils.InitTestMaster(t, "sched-allocatable", nil),
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)

	defer testutils.CleanupTest(t, testCtx)

	cs, ns := testCtx.ClientSet, testCtx.NS.Name
	// Create nodes. First two are small nodes.
	bigNodeName := "fake-node-big"
	nodeNames := []string{"fake-node-small-1", "fake-node-small-2", bigNodeName}
	for _, nodeName := range nodeNames {
		var memory int64 = 200
		if nodeName == bigNodeName {
			memory = 5000
		}
		node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(memory, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(memory, resource.DecimalSI),
		}
		node, err := cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	// Create Pods.
	var pods []*v1.Pod
	podNames := []string{"small-1", "small-2", "small-3", "small-4"}
	pause := imageutils.GetPauseImageName()
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(pause).Obj()
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			},
		}
		pods = append(pods, pod)
	}

	// Make a big pod.
	podNames = append(podNames, "big-1")
	pod := st.MakePod().Namespace(ns).Name("big-1").Container(pause).Obj()
	pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(5000, resource.DecimalSI),
		},
	}
	pods = append(pods, pod)

	// Create the Pods. By default the small pods should land on the small nodes.
	t.Logf("Start to create 5 Pods.")
	for i := range pods {
		t.Logf("Creating Pod %q", pods[i].Name)
		_, err := cs.CoreV1().Pods(ns).Create(context.TODO(), pods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", pods[i].Name, err)
		}
	}

	for i := range pods {
		// Wait for the pod to be scheduled.
		err := wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
			return podScheduled(cs, pods[i].Namespace, pods[i].Name), nil
		})
		if err != nil {
			t.Fatalf("Waiting for pod %q to be scheduled, error: %v", pods[i].Name, err.Error())
		}

		pod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), pods[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// The big pod should be scheduled on the big node.
		if pod.Name == "big-1" {
			if pod.Spec.NodeName == bigNodeName {
				t.Logf("Pod %q is on the big node as expected.", pod.Name)
				continue
			} else {
				t.Errorf("Pod %q is expected on node %q, but found on node %q",
					pod.Name, bigNodeName, pod.Spec.NodeName)
			}
		}

		// The other pods should be scheduled on the small nodes.
		if pod.Spec.NodeName == nodeNames[0] ||
			pod.Spec.NodeName == nodeNames[1] {
			t.Logf("Pod %q is on a small node as expected.", pod.Name)
			continue
		} else {
			t.Errorf("Pod %q is on node %q when it was expected on a small node",
				pod.Name, pod.Spec.NodeName)
		}
	}
}
