/*
Copyright 2021 The Kubernetes Authors.

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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/capacityscheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/controllers"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestElasticController(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := util.NewClientOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	s := scheme.Scheme
	runtime.Must(schedv1alpha1.AddToScheme(s))

	mgrOpts := manager.Options{
		Scheme:             s,
		MetricsBindAddress: "0", // disable metrics to avoid conflicts between packages.
	}
	mgr, err := ctrl.NewManager(globalKubeConfig, mgrOpts)
	if err = (&controllers.ElasticQuotaReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		t.Fatal("unable to create controller", "controller", "ElasticQuota", err)
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

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{capacityscheduling.Name: capacityscheduling.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(300, resource.DecimalSI),
		v1.ResourceCPU:    *resource.NewQuantity(300, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(3000, resource.DecimalSI),
	}
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(300, resource.DecimalSI),
		v1.ResourceCPU:    *resource.NewQuantity(300, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(3000, resource.DecimalSI),
	}
	if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}

	for _, ns := range []string{"ns1", "ns2"} {
		_, err := cs.CoreV1().Namespaces().Create(testCtx.Ctx, &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create integration test ns: %v", err)
		}
	}

	for _, tt := range []struct {
		name          string
		elasticQuotas []*schedv1alpha1.ElasticQuota
		existingPods  []*v1.Pod
		used          []*schedv1alpha1.ElasticQuota
		incomingPods  []*v1.Pod
		want          []*schedv1alpha1.ElasticQuota
	}{
		{
			name: "The status of the pod changes from pending to running",
			elasticQuotas: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t1-eq1").
					Min(MakeResourceList().CPU(100).Mem(1000).Obj()).
					Max(MakeResourceList().CPU(100).Mem(1000).Obj()).Obj(),
				MakeEQ("ns2", "t1-eq2").
					Min(MakeResourceList().CPU(100).Mem(1000).Obj()).
					Max(MakeResourceList().CPU(100).Mem(1000).Obj()).Obj(),
			},
			existingPods: []*v1.Pod{
				MakePod("ns1", "t1-p1").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
				MakePod("ns1", "t1-p2").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t1-p3").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns2", "t1-p4").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
			used: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t1-eq1").
					Used(MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
				MakeEQ("ns2", "t1-eq2").
					Used(MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
			},
			incomingPods: []*v1.Pod{
				MakePod("ns1", "t1-p1").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
				MakePod("ns1", "t1-p2").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t1-p3").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns2", "t1-p4").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},

			want: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t1-eq1").
					Used(MakeResourceList().CPU(30).Mem(40).Obj()).Obj(),
				MakeEQ("ns2", "t1-eq2").
					Used(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
		},
		{
			name: "The status of the pod changes from running to others",
			elasticQuotas: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t2-eq1").
					Min(MakeResourceList().CPU(100).Mem(1000).Obj()).
					Max(MakeResourceList().CPU(100).Mem(1000).Obj()).Obj(),
				MakeEQ("ns2", "t2-eq2").
					Min(MakeResourceList().CPU(100).Mem(1000).Obj()).
					Max(MakeResourceList().CPU(100).Mem(1000).Obj()).Obj(),
			},
			existingPods: []*v1.Pod{
				MakePod("ns1", "t2-p1").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
				MakePod("ns1", "t2-p2").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t2-p3").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns2", "t2-p4").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
			used: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t2-eq1").
					Used(MakeResourceList().CPU(30).Mem(40).Obj()).Obj(),
				MakeEQ("ns2", "t2-eq2").
					Used(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
			incomingPods: []*v1.Pod{
				MakePod("ns1", "t2-p1").Phase(v1.PodSucceeded).Obj(),
				MakePod("ns1", "t2-p3").Phase(v1.PodFailed).Obj(),
			},
			want: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t2-eq1").
					Used(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakeEQ("ns2", "t2-eq2").
					Used(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
		},
		{
			name: "Different resource between max and min",
			elasticQuotas: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t3-eq1").
					Min(MakeResourceList().Mem(1000).Obj()).
					Max(MakeResourceList().CPU(100).Obj()).Obj(),
			},
			existingPods: []*v1.Pod{
				MakePod("ns1", "t3-p1").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
			},
			used: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t3-eq1").
					Used(MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
			},
			incomingPods: []*v1.Pod{
				MakePod("ns1", "t3-p1").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
			},
			want: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t3-eq1").
					Used(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
			},
		},
		{
			name: "EQ doesn't have max and the status of the pod changes from pending to running",
			elasticQuotas: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t4-eq1").
					Min(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
			existingPods: []*v1.Pod{
				MakePod("ns1", "t4-p1").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
				MakePod("ns1", "t4-p2").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t4-p3").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t4-p4").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},
			used: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t4-eq1").
					Used(MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
			},
			incomingPods: []*v1.Pod{
				MakePod("ns1", "t4-p1").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(20).Obj()).Obj(),
				MakePod("ns1", "t4-p2").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t4-p3").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
				MakePod("ns1", "t4-p4").Phase(v1.PodRunning).Node("fake-node").
					Container(MakeResourceList().CPU(10).Mem(10).Obj()).Obj(),
			},

			want: []*schedv1alpha1.ElasticQuota{
				MakeEQ("ns1", "t4-eq1").
					Used(MakeResourceList().CPU(40).Mem(50).Obj()).Obj(),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupElasticQuotas(testCtx.Ctx, extClient, tt.elasticQuotas)
			defer cleanupPods(t, testCtx, tt.existingPods)
			defer cleanupPods(t, testCtx, tt.incomingPods)
			// create elastic quota
			if err := createElasticQuotas(testCtx.Ctx, extClient, tt.elasticQuotas); err != nil {
				t.Fatal(err)
			}

			// create now pod and update status
			for _, pod := range tt.existingPods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
				if pod.Status.Phase == v1.PodRunning {
					_, err = cs.CoreV1().Pods(pod.Namespace).UpdateStatus(testCtx.Ctx, pod, metav1.UpdateOptions{})
					if err != nil {
						t.Fatalf("Failed to update Pod status %q: %v", pod.Name, err)
					}
				}
			}
			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, pod := range tt.incomingPods {
					if !podScheduled(cs, pod.Namespace, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting existPods created error: %v", tt.name, err.Error())
			}

			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, v := range tt.used {
					var eq schedv1alpha1.ElasticQuota
					if err := extClient.Get(testCtx.Ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Name}, &eq); err != nil {
						// This could be a connection error so we want to retry.
						klog.ErrorS(err, "Failed to obtain the elasticQuota clientSet")
						return false, err
					}
					if !quota.Equals(eq.Status.Used, v.Status.Used) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting nowEQUsed error: %v", tt.name, err.Error())
			}

			// update Pods status to check if EQ.used has changed as expected
			for _, pod := range tt.incomingPods {
				if _, err := cs.CoreV1().Pods(pod.Namespace).UpdateStatus(testCtx.Ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Failed to update Pod status %q: %v", pod.Name, err)
				}
			}
			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, pod := range tt.incomingPods {
					if !podScheduled(cs, pod.Namespace, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting nextPods update status error: %v", tt.name, err.Error())
			}

			if err := wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, v := range tt.want {
					var eq schedv1alpha1.ElasticQuota
					if err := extClient.Get(testCtx.Ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Name}, &eq); err != nil {
						// This could be a connection error so we want to retry.
						klog.ErrorS(err, "Failed to obtain the elasticQuota clientSet")
						return false, err
					}
					if !quota.Equals(eq.Status.Used, v.Status.Used) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting nextEQUsed error: %v", tt.name, err.Error())
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}
