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
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

var lowPriority, midPriority, highPriority = int32(0), int32(100), int32(1000)

func TestCoschedulingPlugin(t *testing.T) {
	registry := framework.Registry{coscheduling.Name: coscheduling.New}
	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			QueueSort: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			},
			PreFilter: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
			Permit: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
			Unreserve: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
		},
	}

	testCtx := util.InitTestSchedulerWithOptions(
		t,
		testutils.InitTestMaster(t, "sched-coscheduling", nil),
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)

	defer testutils.CleanupTest(t, testCtx)

	cs, ns := testCtx.ClientSet, testCtx.NS.Name
	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name("fake-node").Label("node", nodeName).Obj()
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node, err := cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}

	// Create Pods belongs to two podGroup.
	var pods []*v1.Pod
	pause := imageutils.GetPauseImageName()

	type podInfo struct {
		podName      string
		podGroupName string
		minAvailable string
		priority     int32
		memReq       int64
	}

	for _, tt := range []struct {
		name         string
		pods         []podInfo
		expectedPods []string
	}{
		{
			name: "equal priority, sequentially pg1 meet min and pg2 not meet min",
			pods: []podInfo{
				{podName: "t1-p1-1", podGroupName: "pg1-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t1-p1-2", podGroupName: "pg1-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t1-p1-3", podGroupName: "pg1-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t1-p2-1", podGroupName: "pg1-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t1-p2-2", podGroupName: "pg1-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t1-p2-3", podGroupName: "pg1-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t1-p2-4", podGroupName: "pg1-2", minAvailable: "4", priority: midPriority, memReq: 100},
			},
			expectedPods: []string{"t1-p1-1", "t1-p1-2", "t1-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and pg2 not meet min",
			pods: []podInfo{
				{podName: "t2-p1-1", podGroupName: "pg2-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t2-p2-1", podGroupName: "pg2-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t2-p1-2", podGroupName: "pg2-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t2-p2-2", podGroupName: "pg2-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t2-p1-3", podGroupName: "pg2-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t2-p2-3", podGroupName: "pg2-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t2-p2-4", podGroupName: "pg2-2", minAvailable: "4", priority: midPriority, memReq: 100},
			},
			expectedPods: []string{"t2-p1-1", "t2-p1-2", "t2-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 not meet min and 3 regular pods",
			pods: []podInfo{
				{podName: "t3-p1-1", podGroupName: "pg3-1", minAvailable: "4", priority: midPriority, memReq: 50},
				{podName: "t3-p2", podGroupName: "", minAvailable: "", priority: midPriority, memReq: 100},
				{podName: "t3-p1-2", podGroupName: "pg3-1", minAvailable: "4", priority: midPriority, memReq: 50},
				{podName: "t3-p3", podGroupName: "", minAvailable: "", priority: midPriority, memReq: 100},
				{podName: "t3-p1-3", podGroupName: "pg3-1", minAvailable: "4", priority: midPriority, memReq: 50},
			},
			expectedPods: []string{"t3-p2", "t3-p3"},
		},
		{
			name: "different priority, sequentially pg1 meet min and pg2 meet min",
			pods: []podInfo{
				{podName: "t4-p1-1", podGroupName: "pg4-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t4-p1-2", podGroupName: "pg4-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t4-p1-3", podGroupName: "pg4-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t4-p2-1", podGroupName: "pg4-2", minAvailable: "3", priority: highPriority, memReq: 50},
				{podName: "t4-p2-2", podGroupName: "pg4-2", minAvailable: "3", priority: highPriority, memReq: 50},
				{podName: "t4-p2-3", podGroupName: "pg4-2", minAvailable: "3", priority: highPriority, memReq: 50},
			},
			expectedPods: []string{"t4-p2-1", "t4-p2-2", "t4-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and pg2 meet min",
			pods: []podInfo{
				{podName: "t5-p1-1", podGroupName: "pg5-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t5-p2-1", podGroupName: "pg5-2", minAvailable: "3", priority: highPriority, memReq: 50},
				{podName: "t5-p1-2", podGroupName: "pg5-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t5-p2-2", podGroupName: "pg5-2", minAvailable: "3", priority: highPriority, memReq: 50},
				{podName: "t5-p1-3", podGroupName: "pg5-1", minAvailable: "3", priority: midPriority, memReq: 100},
				{podName: "t5-p2-3", podGroupName: "pg5-2", minAvailable: "3", priority: highPriority, memReq: 50},
			},
			expectedPods: []string{"t5-p2-1", "t5-p2-2", "t5-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and 3 regular pods",
			pods: []podInfo{
				{podName: "t6-p1-1", podGroupName: "pg6-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t6-p2", podGroupName: "", minAvailable: "", priority: highPriority, memReq: 100},
				{podName: "t6-p1-2", podGroupName: "pg6-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t6-p3", podGroupName: "", minAvailable: "", priority: highPriority, memReq: 100},
				{podName: "t6-p1-3", podGroupName: "pg6-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t6-p4", podGroupName: "", minAvailable: "", priority: highPriority, memReq: 100},
			},
			expectedPods: []string{"t6-p2", "t6-p3", "t6-p4"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and p2 p3 not meet min",
			pods: []podInfo{
				{podName: "t7-p1-1", podGroupName: "pg7-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t7-p2-1", podGroupName: "pg7-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p3-1", podGroupName: "pg7-3", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p1-2", podGroupName: "pg7-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t7-p2-2", podGroupName: "pg7-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p3-2", podGroupName: "pg7-3", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p1-3", podGroupName: "pg7-1", minAvailable: "3", priority: midPriority, memReq: 50},
				{podName: "t7-p2-3", podGroupName: "pg7-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p3-3", podGroupName: "pg7-3", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p2-4", podGroupName: "pg7-2", minAvailable: "4", priority: midPriority, memReq: 100},
				{podName: "t7-p3-4", podGroupName: "pg7-3", minAvailable: "4", priority: midPriority, memReq: 100},
			},
			expectedPods: []string{"t7-p1-1", "t7-p1-2", "t7-p1-3"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-coscheduling-test %v", tt.name)
			testutils.CleanupPods(cs, t, pods)
			pods = make([]*v1.Pod, 0)
			for _, podInfo := range tt.pods {
				pod := st.MakePod().Namespace(ns).Name(podInfo.podName).Container(pause).
					Priority(podInfo.priority).ZeroTerminationGracePeriod().Obj()
				pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(podInfo.memReq, resource.DecimalSI),
					},
				}
				pod.Labels = map[string]string{
					coscheduling.PodGroupName:         podInfo.podGroupName,
					coscheduling.PodGroupMinAvailable: podInfo.minAvailable,
				}
				pods = append(pods, pod)
			}

			// Create Pods, We will expect them to be scheduled in a reversed order.
			for i := range pods {
				_, err := cs.CoreV1().Pods(pods[i].Namespace).Create(testCtx.Ctx, pods[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pods[i].Name, err)
				}
			}

			// A workaround to speed up the test by moving unschedulable Pods back to activeQ.
			wait.PollUntil(time.Millisecond*200, func() (bool, error) {
				numUnschedulablePods := testCtx.Scheduler.SchedulingQueue.NumUnschedulablePods()
				return numUnschedulablePods > 0, nil
			}, testCtx.Ctx.Done())
			klog.Infof("Start to move unschedulable Pods back to activeQ.")
			// DEBUG
			for _, p := range testCtx.Scheduler.SchedulingQueue.PendingPods() {
				klog.Infof("[DEBUG] pod %v is pending", p.Name)
			}
			testCtx.Scheduler.SchedulingQueue.MoveAllToActiveOrBackoffQueue("fake event")

			err = wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, ns, v) {
						t.Logf("waiting pod failed %v", v)
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
		})
	}
}

// podScheduled returns true if a node is assigned to the given pod.
func podScheduled(c clientset.Interface, podNamespace, podName string) bool {
	pod, err := c.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		// This could be a connection error so we want to retry.
		return false
	}
	if pod.Spec.NodeName == "" {
		return false
	}
	return true
}
