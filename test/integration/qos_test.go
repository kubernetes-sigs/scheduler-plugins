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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/qos"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestQOSPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: qos.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{qos.Name: qos.New}),
	)
	syncInformerFactory(testCtx)
	// Do not start the scheduler.
	// go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name("fake-node").Label("node", nodeName).Obj()
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(500, resource.DecimalSI),
	}
	node, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}

	// Create 3 Pods.
	var pods []*v1.Pod
	podNames := []string{"bestefforts", "burstable", "guaranteed"}
	pause := imageutils.GetPauseImageName()
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(pause).Obj()
		pods = append(pods, pod)
	}
	// Make pods[0] BestEfforts (i.e., do nothing).
	// Make pods[1] Burstable.
	pods[1].Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
		},
		Limits: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
		},
	}
	// Make pods[2] Guaranteed.
	pods[2].Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
		},
		Limits: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
		},
	}

	// Create 3 Pods with the order: BestEfforts, Burstable, Guaranteed.
	// We will expect them to be scheduled in a reversed order.
	t.Logf("Start to create 3 Pods.")
	for i := range pods {
		t.Logf("Creating Pod %q", pods[i].Name)
		_, err = cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", pods[i].Name, err)
		}
	}
	defer cleanupPods(t, testCtx, pods)

	// Wait for all Pods are in the scheduling queue.
	err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
		pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
		if len(pendingPods) == len(pods) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Expect Pods are popped in the QoS class order.
	logger := klog.FromContext(testCtx.Ctx)
	for i := len(podNames) - 1; i >= 0; i-- {
		podInfo, _ := testCtx.Scheduler.NextPod(logger)
		if podInfo.Pod.Name != podNames[i] {
			t.Errorf("Expect Pod %q, but got %q", podNames[i], podInfo.Pod.Name)
		} else {
			t.Logf("Pod %q is popped out as expected.", podInfo.Pod.Name)
		}
	}
}
