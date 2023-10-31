/*
Copyright 2023 The Kubernetes Authors.

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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/controllers"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	"sigs.k8s.io/scheduler-plugins/test/util"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestPodGroupController(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := util.NewClientOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	s := scheme.Scheme

	runtime.Must(v1alpha1.AddToScheme(s))

	mgrOpts := manager.Options{
		Scheme:             s,
		MetricsBindAddress: "0", // disable metrics to avoid conflicts between packages.
	}

	mgr, _ := ctrl.NewManager(globalKubeConfig, mgrOpts)
	if err := (&controllers.PodGroupReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		t.Fatal("unable to create controller", "controller", "PodGroup", err)
	}

	go func() {
		if err := mgr.Start(signalHandler); err != nil {
			panic(err)
		}
	}()

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == scheduling.GroupName {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{coscheduling.Name: coscheduling.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}
	ignoreOpts := cmpopts.IgnoreFields(v1alpha1.PodGroupStatus{}, "ScheduleStartTime")
	// TODO: Update the number of scheduled pods when changing the Reconcile logic.
	// PostBind is not running in this test, so the number of Scheduled pods in PodGroup is 0.
	for _, tt := range []struct {
		name                string
		podGroups           []*v1alpha1.PodGroup
		existingPods        []*v1.Pod
		intermediatePGState []*v1alpha1.PodGroup
		incomingPods        []*v1.Pod
		expectedPGState     []*v1alpha1.PodGroup
	}{
		{
			name: "Statuses of all pods change from pending to running",
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Obj(),
			},
			intermediatePGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Scheduling", "", 0, 0, 0, 0),
			},
			incomingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
			},
			expectedPGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Running", "", 0, 3, 0, 0),
			},
		},
		{
			name: "Statuses of all pods change from running to succeeded",
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
			},
			intermediatePGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Running", "", 0, 3, 0, 0),
			},
			incomingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodSucceeded).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodSucceeded).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodSucceeded).Obj(),
			},
			expectedPGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Finished", "", 0, 0, 3, 0),
			},
		},
		{
			name: "The status of one pod changes from running to succeeded",
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
			},
			intermediatePGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Running", "", 0, 3, 0, 0),
			},
			incomingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodSucceeded).Obj(),
			},
			expectedPGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Running", "", 0, 2, 1, 0),
			},
		},
		{
			name: "The status of the pod changes from running to failed",
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodRunning).Obj(),
			},
			intermediatePGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Running", "", 0, 3, 0, 0),
			},
			incomingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").Node(nodeName).Phase(v1.PodFailed).Obj(),
			},
			expectedPGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Failed", "", 0, 2, 0, 1),
			},
		},
		{
			name: "The status of no podGroup label pod changes from pending to running",
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Obj(),
			},
			intermediatePGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Pending", "", 0, 0, 0, 0),
			},
			incomingPods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Phase(v1.PodRunning).Obj(),
				st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Node(nodeName).Phase(v1.PodRunning).Obj(),
			},
			expectedPGState: []*v1alpha1.PodGroup{
				util.UpdatePGStatus(util.MakePG("pg1-1", ns, 3, nil, nil), "Pending", "", 0, 0, 0, 0),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupPodGroups(testCtx.Ctx, extClient, tt.podGroups)
			defer cleanupPods(t, testCtx, tt.existingPods)
			defer cleanupPods(t, testCtx, tt.incomingPods)
			// create pod group
			if err := createPodGroups(testCtx.Ctx, extClient, tt.podGroups); err != nil {
				t.Fatal(err)
			}

			// create Pods
			for _, pod := range tt.existingPods {
				klog.InfoS("Creating pod ", "podName", pod.Name)
				if _, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
				if pod.Status.Phase == v1.PodRunning {
					if _, err := cs.CoreV1().Pods(pod.Namespace).UpdateStatus(testCtx.Ctx, pod, metav1.UpdateOptions{}); err != nil {
						t.Fatalf("Failed to update Pod status %q: %v", pod.Name, err)
					}
				}
			}
			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, pod := range tt.incomingPods {
					if !podScheduled(cs, ns, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting existPods create error: %v", tt.name, err.Error())
			}

			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, v := range tt.intermediatePGState {
					var pg v1alpha1.PodGroup
					if err := extClient.Get(testCtx.Ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Name}, &pg); err != nil {
						// This could be a connection error so we want to retry.
						klog.ErrorS(err, "Failed to obtain the PodGroup clientSet")
						return false, err
					}
					if diff := gocmp.Diff(pg.Status, v.Status, ignoreOpts); diff != "" {
						t.Error(diff)
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting now PodGroup Status error: %v", tt.name, err.Error())
			}

			// update Pods status to check if PodGroup.Status has changed as expected
			for _, pod := range tt.incomingPods {
				if _, err := cs.CoreV1().Pods(pod.Namespace).UpdateStatus(testCtx.Ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Failed to update Pod status %q: %v", pod.Name, err)
				}
			}
			// wait for all incomingPods to be scheduled
			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, pod := range tt.incomingPods {
					if !podScheduled(cs, pod.Namespace, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting incomingPods scheduled error: %v", tt.name, err.Error())
			}

			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPGState {
					var pg v1alpha1.PodGroup
					if err := extClient.Get(testCtx.Ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Name}, &pg); err != nil {
						// This could be a connection error so we want to retry.
						klog.ErrorS(err, "Failed to obtain the PodGroup clientSet")
						return false, err
					}

					if diff := gocmp.Diff(pg.Status, v.Status, ignoreOpts); diff != "" {
						t.Error(diff)
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting PodGroup status update error: %v", tt.name, err.Error())
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}
