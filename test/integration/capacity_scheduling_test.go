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
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutil "k8s.io/kubernetes/test/integration/util"

	schedconfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/capacityscheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

const ResourceGPU v1.ResourceName = "nvidia.com/gpu"

func TestCapacityScheduling(t *testing.T) {
	todo := context.TODO()
	ctx, cancelFunc := context.WithCancel(todo)
	testCtx := &testutil.TestContext{
		Ctx:      ctx,
		CancelFn: cancelFunc,
		CloseFn:  func() {},
	}

	registry := fwkruntime.Registry{capacityscheduling.Name: capacityscheduling.New}
	t.Log("create apiserver")
	_, config := util.StartApi(t, todo.Done())

	config.ContentType = "application/json"

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	kubeConfigPath := util.BuildKubeConfigFile(config)
	if len(kubeConfigPath) == 0 {
		t.Fatal("Build KubeConfigFile failed")
	}
	defer os.RemoveAll(kubeConfigPath)

	t.Log("create crd")
	if _, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, makeElasticQuotaCRD(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	cs := kubernetes.NewForConfigOrDie(config)
	extClient := versioned.NewForConfigOrDie(config)

	if err = wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == scheduling.GroupName {
				return true, nil
			}
		}
		t.Log("waiting for crd api ready")
		return false, nil
	}); err != nil {
		t.Fatalf("Waiting for crd read time out: %v", err)
	}

	cfg := &schedconfig.CapacitySchedulingArgs{
		KubeConfigPath: kubeConfigPath,
	}

	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			PreFilter: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: capacityscheduling.Name},
				},
			},
			PostFilter: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: capacityscheduling.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			},
			Reserve: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: capacityscheduling.Name},
				},
			},
		},
		PluginConfig: []schedapi.PluginConfig{
			{
				Name: capacityscheduling.Name,
				Args: cfg,
			},
		},
	}

	testCtx.ClientSet = cs
	testCtx = util.InitTestSchedulerWithOptions(
		t,
		testCtx,
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)
	t.Log("init scheduler success")
	defer testutil.CleanupTest(t, testCtx)

	for _, nodeName := range []string{"fake-node-1", "fake-node-2"} {
		node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(100, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(100, resource.DecimalSI),
		}
		node, err = cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	for _, ns := range []string{"ns1", "ns2"} {
		_, err := cs.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			t.Fatalf("Failed to integration test ns: %v", err)
		}
		autoCreate := false
		t.Logf("namespaces %+v", ns)
		_, err = cs.CoreV1().ServiceAccounts(ns).Create(ctx, &v1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}, AutomountServiceAccountToken: &autoCreate}, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			t.Fatalf("Failed to create ns default: %v", err)
		}
	}

	for _, tt := range []struct {
		name          string
		namespaces    []string
		existPods     []*v1.Pod
		addPods       []*v1.Pod
		elasticQuotas []*v1alpha1.ElasticQuota
		expectedPods  []*v1.Pod
	}{
		{
			name:       "cross-namespace preemption",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t1-p1", "ns1", 50, 10, midPriority, "t1-p1", "fake-node-1"),
				util.MakePod("t1-p2", "ns1", 50, 10, midPriority, "t1-p2", "fake-node-1"),
				util.MakePod("t1-p3", "ns1", 50, 10, lowPriority, "t1-p3", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t1-p4", "ns2", 50, 10, midPriority, "t1-p4", ""),
				util.MakePod("t1-p5", "ns2", 50, 10, midPriority, "t1-p5", ""),
				util.MakePod("t1-p6", "ns2", 50, 10, lowPriority, "t1-p6", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t1-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t1-p2", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t1-p4", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t1-p5", Namespace: "ns2"},
				},
			},
		},
		{
			name:       "in-namespace preemption",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t2-p1", "ns1", 50, 10, midPriority, "t2-p1", "fake-node-1"),
				util.MakePod("t2-p2", "ns1", 50, 10, lowPriority, "t2-p2", "fake-node-1"),
				util.MakePod("t2-p3", "ns2", 50, 10, midPriority, "t2-p3", "fake-node-2"),
				util.MakePod("t2-p4", "ns2", 50, 10, lowPriority, "t2-p4", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t2-p5", "ns1", 50, 10, midPriority, "t2-p5", ""),
				util.MakePod("t2-p6", "ns2", 50, 10, midPriority, "t2-p6", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t2-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t2-p3", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t2-p5", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t2-p6", Namespace: "ns2"},
				},
			},
		},
		{
			name:       "regular preemption without Elastic Quota",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t3-p1", "ns1", 50, 10, highPriority, "t3-p1", "fake-node-1"),
				util.MakePod("t3-p2", "ns1", 50, 10, lowPriority, "t3-p2", "fake-node-1"),
				util.MakePod("t3-p3", "ns2", 50, 10, highPriority, "t3-p3", "fake-node-2"),
				util.MakePod("t3-p4", "ns2", 50, 10, lowPriority, "t3-p4", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t3-p5", "ns1", 50, 10, midPriority, "t3-p5", ""),
				util.MakePod("t3-p6", "ns2", 50, 10, midPriority, "t3-p6", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t3-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t3-p3", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t3-p5", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t3-p6", Namespace: "ns2"},
				},
			},
		},
		{
			name:       "in-namespace preemption failed because it can't find node which is suitable for preemption",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t4-p1", "ns1", 50, 10, midPriority, "t4-p1", "fake-node-1"),
				util.MakePod("t4-p2", "ns1", 50, 10, midPriority, "t4-p2", "fake-node-1"),
				util.MakePod("t4-p3", "ns1", 50, 10, lowPriority, "t4-p3", "fake-node-2"),
				util.MakePod("t4-p4", "ns2", 50, 10, midPriority, "t4-p4", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t4-p5", "ns1", 150, 10, highPriority, "t4-p5", ""),
				util.MakePod("t4-p6", "ns2", 150, 10, highPriority, "t4-p5", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t4-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t4-p2", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t4-p3", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t4-p4", Namespace: "ns2"},
				},
			},
		},
		{
			name:       "pod subjects to overused quota can't preempt pods subjects to other quotas",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t5-p1", "ns1", 50, 10, midPriority, "t5-p1", "fake-node-1"),
				util.MakePod("t5-p2", "ns2", 50, 10, highPriority, "t5-p2", "fake-node-1"),
				util.MakePod("t5-p3", "ns2", 50, 10, highPriority, "t5-p3", "fake-node-2"),
				util.MakePod("t5-p4", "ns2", 50, 10, highPriority, "t5-p4", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t5-p5", "ns2", 50, 10, highPriority, "t5-p5", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t5-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t5-p2", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t5-p3", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t5-p4", Namespace: "ns2"},
				},
			},
		},
		{
			name:       "cross-node preemption isn't supported",
			namespaces: []string{"ns1", "ns2"},
			existPods: []*v1.Pod{
				util.MakePod("t6-p1", "ns1", 50, 10, lowPriority, "t6-p1", "fake-node-1"),
				util.MakePod("t6-p2", "ns1", 50, 10, lowPriority, "t6-p2", "fake-node-2"),
				util.MakePod("t6-p3", "ns2", 50, 10, midPriority, "t6-p3", "fake-node-1"),
				util.MakePod("t6-p4", "ns2", 50, 10, midPriority, "t6-p4", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t6-p5", "ns1", 100, 20, highPriority, "t6-p5", ""),
				util.MakePod("t6-p6", "ns2", 100, 20, highPriority, "t6-p5", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
				{
					TypeMeta:   metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(200, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t6-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t6-p2", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t6-p3", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t6-p4", Namespace: "ns2"},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupElasticQuotas(ctx, extClient, tt.elasticQuotas)
			defer testutil.CleanupPods(cs, t, tt.existPods)
			defer testutil.CleanupPods(cs, t, tt.addPods)

			if err := createElasticQuotas(ctx, extClient, tt.elasticQuotas); err != nil {
				t.Fatal(err)
			}

			for _, pod := range tt.existPods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			err = wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, pod := range tt.existPods {
					if !podScheduled(cs, pod.Namespace, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting existPods created error: %v", tt.name, err.Error())
			}

			// Create Pods, We will expect them to be scheduled in a reversed order.
			for _, pod := range tt.addPods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			err = wait.Poll(time.Millisecond*200, 10*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, v.Namespace, v.Name) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
			t.Logf("case %v finished", tt.name)
		})
	}
}

func makeElasticQuotaCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "elasticquotas.scheduling.sigs.k8s.io",
			Annotations: map[string]string{
				"api-approved.kubernetes.io": "https://github.com/kubernetes-sigs/scheduler-plugins/pull/50",
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: scheduling.GroupName,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:       "ElasticQuota",
				Plural:     "elasticquotas",
				Singular:   "elasticquota",
				ShortNames: []string{"eq", "eqs"},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"min": {
										Type: "object",
										AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
											Schema: &apiextensionsv1.JSONSchemaProps{
												AnyOf: []apiextensionsv1.JSONSchemaProps{
													{
														Type: "integer",
													},
													{
														Type: "string",
													},
												},
												XIntOrString: true,
											},
										},
									},
									"max": {
										Type: "object",
										AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
											Schema: &apiextensionsv1.JSONSchemaProps{
												AnyOf: []apiextensionsv1.JSONSchemaProps{
													{
														Type: "integer",
													},
													{
														Type: "string",
													},
												},
												XIntOrString: true,
											},
										},
									},
								},
							},
							"status": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"used": {
										Type: "object",
										AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
											Schema: &apiextensionsv1.JSONSchemaProps{
												AnyOf: []apiextensionsv1.JSONSchemaProps{
													{
														Type: "integer",
													},
													{
														Type: "string",
													},
												},
												XIntOrString: true,
											},
										}},
								},
							},
						},
					},
				}}},
		},
	}
}

func createElasticQuotas(ctx context.Context, client versioned.Interface, elasticQuotas []*v1alpha1.ElasticQuota) error {
	for _, eq := range elasticQuotas {
		_, err := client.SchedulingV1alpha1().ElasticQuotas(eq.Namespace).Create(ctx, eq, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupElasticQuotas(ctx context.Context, client versioned.Interface, elasticQuotas []*v1alpha1.ElasticQuota) {
	for _, eq := range elasticQuotas {
		err := client.SchedulingV1alpha1().ElasticQuotas(eq.Namespace).Delete(ctx, eq.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("clean up ElasticQuota (%v/%v) error %s", eq.Namespace, eq.Name, err.Error())
		}
	}
}
