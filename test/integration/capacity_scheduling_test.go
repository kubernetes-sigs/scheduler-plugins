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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/capacityscheduling"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestCapacityScheduling(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := util.NewClientOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
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

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled, schedapi.Plugin{Name: capacityscheduling.Name})
	cfg.Profiles[0].Plugins.PostFilter = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: capacityscheduling.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.Reserve.Enabled = append(cfg.Profiles[0].Plugins.Reserve.Enabled, schedapi.Plugin{Name: capacityscheduling.Name})

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{capacityscheduling.Name: capacityscheduling.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

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
		if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	for _, ns := range []string{"ns1", "ns2", "ns3"} {
		createNamespace(t, testCtx, ns)
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
		{
			name:       "cross-namespace preemption with three elasticquota",
			namespaces: []string{"ns1", "ns2", "ns3"},
			existPods: []*v1.Pod{
				util.MakePod("t7-p1", "ns1", 0, 1, highPriority, "t7-p1", "fake-node-1"),
				util.MakePod("t7-p2", "ns1", 0, 1, midPriority, "t7-p2", "fake-node-2"),
				util.MakePod("t7-p3", "ns1", 0, 1, midPriority, "t7-p3", "fake-node-2"),
				util.MakePod("t7-p5", "ns2", 0, 1, midPriority, "t7-p5", "fake-node-2"),
				util.MakePod("t7-p6", "ns2", 0, 1, midPriority, "t7-p6", "fake-node-1"),
				util.MakePod("t7-p7", "ns2", 0, 1, midPriority, "t7-p7", "fake-node-2"),
			},
			addPods: []*v1.Pod{
				util.MakePod("t7-p9", "ns3", 0, 1, midPriority, "t7-p9", ""),
				util.MakePod("t7-p10", "ns3", 0, 1, midPriority, "t7-p10", ""),
				util.MakePod("t7-p11", "ns3", 0, 1, midPriority, "t7-p11", ""),
				util.MakePod("t7-p12", "ns3", 0, 1, midPriority, "t7-p12", ""),
			},
			elasticQuotas: []*v1alpha1.ElasticQuota{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "eq1", Namespace: "ns1"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(1, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(3, resource.DecimalSI),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "eq2", Namespace: "ns2"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(3, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(3, resource.DecimalSI),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "eq3", Namespace: "ns3"},
					Spec: v1alpha1.ElasticQuotaSpec{
						Min: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(100, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(3, resource.DecimalSI),
						},
						Max: v1.ResourceList{
							v1.ResourceMemory: *resource.NewQuantity(200, resource.DecimalSI),
							v1.ResourceCPU:    *resource.NewMilliQuantity(4, resource.DecimalSI),
						},
					},
				},
			},
			expectedPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p1", Namespace: "ns1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p5", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p6", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p7", Namespace: "ns2"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p9", Namespace: "ns3"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p10", Namespace: "ns3"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "t7-p11", Namespace: "ns3"},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupElasticQuotas(testCtx.Ctx, extClient, tt.elasticQuotas)
			defer cleanupPods(t, testCtx, tt.existPods)
			defer cleanupPods(t, testCtx, tt.addPods)

			if err := createElasticQuotas(testCtx.Ctx, extClient, tt.elasticQuotas); err != nil {
				t.Fatal(err)
			}

			for _, pod := range tt.existPods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			if err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, 10*time.Second, false, func(ctx context.Context) (bool, error) {
				for _, pod := range tt.existPods {
					if !podScheduled(cs, pod.Namespace, pod.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting existPods created error: %v", tt.name, err.Error())
			}

			// Create Pods, we will expect them to be scheduled in a reversed order.
			for _, pod := range tt.addPods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			if err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, 10*time.Second, false, func(ctx context.Context) (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, v.Namespace, v.Name) {
						return false, nil
					}
				}
				return true, nil
			}); err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}

func createElasticQuotas(ctx context.Context, client client.Client, elasticQuotas []*v1alpha1.ElasticQuota) error {
	for _, eq := range elasticQuotas {
		if err := client.Create(ctx, eq); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupElasticQuotas(ctx context.Context, client client.Client, elasticQuotas []*v1alpha1.ElasticQuota) {
	for _, eq := range elasticQuotas {
		_ = client.Delete(ctx, eq)
	}
}
