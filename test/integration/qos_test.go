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
	"reflect"
	"strings"
	"sync"
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

func TestQOSPluginSuite(t *testing.T) {
	t.Run("DifferentQoS", func(t *testing.T) {
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
				v1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
			},
		}
		// Make pods[2] Guaranteed.
		pods[2].Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			},
			Limits: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			},
		}

		// Create 3 Pods with the order: BestEfforts, Burstable, Guaranteed.
		// We will expect them to be scheduled in a reversed order.
		t.Logf("Start to create 3 Pods.")
		// Concurrently create all Pods.
		var wg sync.WaitGroup
		for _, pod := range pods {
			wg.Add(1)
			go func(p *v1.Pod) {
				defer wg.Done()
				_, err = cs.CoreV1().Pods(ns).Create(testCtx.Ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Errorf("Failed to create Pod %q: %v", p.Name, err)
				} else {
					t.Logf("Created Pod %q", p.Name)
				}
			}(pod)
		}
		wg.Wait()
		defer cleanupPods(t, testCtx, pods)

		// Wait for all Pods are in the scheduling queue.
		err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
			pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
			if len(pendingPods) == len(pods) {
				// Collect Pod names into a slice.
				podNames := make([]string, len(pendingPods))
				for i, podInfo := range pendingPods {
					podNames[i] = podInfo.Name
				}
				t.Logf("All Pods are in the pending queue: %v", strings.Join(podNames, ", "))
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Expect Pods are popped in the QoS class order.
		logger := klog.FromContext(testCtx.Ctx)
		expectedOrder := []string{"guaranteed", "burstable", "bestefforts"}
		actualOrder := make([]string, len(expectedOrder))
		for i := 0; i < len(expectedOrder); i++ {
			podInfo, _ := testCtx.Scheduler.NextPod(logger)
			actualOrder[i] = podInfo.Pod.Name
			t.Logf("Popped Pod %q", podInfo.Pod.Name)
		}
		if !reflect.DeepEqual(actualOrder, expectedOrder) {
			t.Errorf("Expected Pod order %v, but got %v", expectedOrder, actualOrder)
		} else {
			t.Logf("Pods were popped out in the expected order.")
		}
	})
	t.Run("SameQoSDifferentCreationTime", func(t *testing.T) {
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
		defer cleanupTest(t, testCtx)

		ns := fmt.Sprintf("integration-test-same-qos-%v", string(uuid.NewUUID()))
		createNamespace(t, testCtx, ns)

		// Create a Node.
		nodeName := "fake-node"
		node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(500, resource.DecimalSI),
		}
		node, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}

		// Create 3 Pods with the same QoS class (Guaranteed) but different creation times.
		var pods []*v1.Pod
		podNames := []string{"guaranteed-1", "guaranteed-2", "guaranteed-3"}
		pause := imageutils.GetPauseImageName()
		for i := 0; i < len(podNames); i++ {
			pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(pause).Obj()
			pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
					v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
				},
			}
			pods = append(pods, pod)
		}

		// Create Pods sequentially with a delay between each creation to ensure different creation timestamps.
		t.Logf("Start to create 3 Guaranteed Pods sequentially.")
		for _, pod := range pods {
			_, err = cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
			if err != nil {
				t.Errorf("Failed to create Pod %q: %v", pod.Name, err)
			} else {
				t.Logf("Created Pod %q", pod.Name)
			}
		}
		defer cleanupPods(t, testCtx, pods)

		// Wait for all Pods are in the scheduling queue.
		err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
			pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
			if len(pendingPods) == len(pods) {
				// Collect Pod names into a slice.
				podNames := make([]string, len(pendingPods))
				for i, podInfo := range pendingPods {
					podNames[i] = podInfo.Name
				}
				t.Logf("All Pods are in the pending queue: %v", strings.Join(podNames, ", "))
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Expect Pods are popped in the order of their creation time (earliest first).
		logger := klog.FromContext(testCtx.Ctx)
		expectedOrder := podNames
		actualOrder := make([]string, len(expectedOrder))
		for i := 0; i < len(expectedOrder); i++ {
			podInfo, _ := testCtx.Scheduler.NextPod(logger)
			actualOrder[i] = podInfo.Pod.Name
			t.Logf("Popped Pod %q", podInfo.Pod.Name)
		}
		if !reflect.DeepEqual(actualOrder, expectedOrder) {
			t.Errorf("Expected Pod order %v, but got %v", expectedOrder, actualOrder)
		} else {
			t.Logf("Pods were popped out in the expected order based on creation time.")
		}
	})
}
