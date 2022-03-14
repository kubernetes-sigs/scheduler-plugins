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

package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	schedfake "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	testutil "sigs.k8s.io/scheduler-plugins/test/integration"
)

func TestElasticQuotaController_Run(t *testing.T) {
	ctx := context.TODO()
	cases := []struct {
		name          string
		elasticQuotas []*v1alpha1.ElasticQuota
		pods          []*v1.Pod
		want          []*v1alpha1.ElasticQuota
	}{
		{
			name: "no init Containers pod",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t1-ns1", "t1-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).GPU(1).Obj()).Obj(),
			},
			pods: []*v1.Pod{
				testutil.MakePod("t1-ns1", "pod1").Phase(v1.PodRunning).Container(
					testutil.MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Obj(),
				testutil.MakePod("t1-ns1", "pod2").Phase(v1.PodPending).Container(
					testutil.MakeResourceList().CPU(1).Mem(2).GPU(0).Obj()).Obj(),
			},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t1-ns1", "t1-eq1").
					Used(testutil.MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).Obj(),
			},
		},
		{
			name: "have init Containers pod",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t2-ns1", "t2-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).Obj()).Obj(),
			},

			pods: []*v1.Pod{
				// CPU: 2, Mem: 4
				testutil.MakePod("t2-ns1", "pod1").Phase(v1.PodRunning).
					Container(
						testutil.MakeResourceList().CPU(1).Mem(2).Obj()).
					Container(
						testutil.MakeResourceList().CPU(1).Mem(2).Obj()).Obj(),
				// CPU: 3, Mem: 3
				testutil.MakePod("t2-ns1", "pod2").Phase(v1.PodRunning).
					InitContainerRequest(
						testutil.MakeResourceList().CPU(2).Mem(1).Obj()).
					InitContainerRequest(
						testutil.MakeResourceList().CPU(2).Mem(3).Obj()).
					Container(
						testutil.MakeResourceList().CPU(2).Mem(1).Obj()).
					Container(
						testutil.MakeResourceList().CPU(1).Mem(1).Obj()).Obj(),
			},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t2-ns1", "t2-eq1").
					Used(testutil.MakeResourceList().CPU(5).Mem(7).Obj()).Obj(),
			},
		},
		{
			name: "update pods",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t3-ns1", "t3-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).Obj()).Obj(),
				testutil.MakeEQ("t3-ns2", "t3-eq2").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).Obj()).Obj(),
			},
			pods: []*v1.Pod{
				// CPU: 2, Mem: 4
				testutil.MakePod("t3-ns1", "pod1").Phase(v1.PodRunning).
					Container(testutil.MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).
					Container(testutil.MakeResourceList().CPU(1).Mem(2).Obj()).Obj(),
				// CPU: 3, Mem: 3
				testutil.MakePod("t3-ns1", "pod1").Phase(v1.PodPending).
					InitContainerRequest(testutil.MakeResourceList().CPU(2).Mem(1).Obj()).
					InitContainerRequest(testutil.MakeResourceList().CPU(2).Mem(3).Obj()).
					Container(testutil.MakeResourceList().CPU(2).Mem(1).Obj()).
					Container(testutil.MakeResourceList().CPU(1).Mem(1).Obj()).Obj(),
				// CPU: 4, Mem: 3
				testutil.MakePod("t3-ns2", "pod2").Phase(v1.PodRunning).
					InitContainerRequest(testutil.MakeResourceList().CPU(2).Mem(1).Obj()).
					InitContainerRequest(testutil.MakeResourceList().CPU(2).Mem(3).Obj()).
					Container(testutil.MakeResourceList().CPU(3).Mem(1).Obj()).
					Container(testutil.MakeResourceList().CPU(1).Mem(1).Obj()).Obj(),
			},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t3-ns1", "t3-eq1").
					Used(testutil.MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
				testutil.MakeEQ("t3-ns2", "t3-eq2").
					Used(testutil.MakeResourceList().CPU(4).Mem(3).Obj()).Obj(),
			},
		},
		{
			name: "min and max have the same fields",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t4-ns1", "t4-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).Obj()).Obj(),
			},
			pods: []*v1.Pod{},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t4-ns1", "t4-eq1").
					Used(testutil.MakeResourceList().CPU(0).Mem(0).Obj()).Obj(),
			},
		},
		{
			name: "min and max have the different fields",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t5-ns1", "t5-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).GPU(2).Obj()).
					Max(testutil.MakeResourceList().CPU(5).Mem(15).Obj()).Obj(),
			},
			pods: []*v1.Pod{},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t5-ns1", "t5-eq1").
					Used(testutil.MakeResourceList().CPU(0).Mem(0).GPU(0).Obj()).Obj(),
			},
		},
		{
			name: "pod and eq in the different namespaces",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t6-ns1", "t6-eq1").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).GPU(2).Obj()).
					Max(testutil.MakeResourceList().CPU(50).Mem(15).Obj()).Obj(),
				testutil.MakeEQ("t6-ns2", "t6-eq2").
					Min(testutil.MakeResourceList().CPU(3).Mem(5).GPU(2).Obj()).
					Max(testutil.MakeResourceList().CPU(50).Mem(15).Obj()).Obj(),
			},
			pods: []*v1.Pod{
				testutil.MakePod("t6-ns3", "pod1").Phase(v1.PodRunning).
					Container(testutil.MakeResourceList().CPU(1).Mem(2).GPU(1).Obj()).
					Container(testutil.MakeResourceList().CPU(1).Mem(2).Obj()).Obj(),
			},
			want: []*v1alpha1.ElasticQuota{
				testutil.MakeEQ("t6-ns1", "t6-eq1").
					Used(testutil.MakeResourceList().CPU(0).Mem(0).GPU(0).Obj()).Obj(),
				testutil.MakeEQ("t6-ns2", "t6-eq2").
					Used(testutil.MakeResourceList().CPU(0).Mem(0).GPU(0).Obj()).Obj(),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			schedClient := schedfake.NewSimpleClientset()
			for _, v := range c.elasticQuotas {
				schedClient.Tracker().Add(v)
			}
			informerFactory := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
			pgInformerFactory := schedinformer.NewSharedInformerFactory(schedClient, controller.NoResyncPeriodFunc())
			podInformer := informerFactory.Core().V1().Pods()
			eqInformer := pgInformerFactory.Scheduling().V1alpha1().ElasticQuotas()
			ctrl := NewElasticQuotaController(kubeClient, eqInformer, podInformer, schedClient, WithFakeRecorder(3))

			pgInformerFactory.Start(ctx.Done())
			informerFactory.Start(ctx.Done())
			// 0 means not set
			for _, p := range c.pods {
				kubeClient.Tracker().Add(p)
				kubeClient.CoreV1().Pods(p.Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
			}

			go ctrl.Run(1, ctx.Done())
			err := wait.Poll(200*time.Millisecond, 1*time.Second, func() (done bool, err error) {
				for _, v := range c.want {
					get, err := schedClient.SchedulingV1alpha1().ElasticQuotas(v.Namespace).Get(ctx, v.Name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					if !quota.Equals(get.Status.Used, v.Status.Used) {
						return false, fmt.Errorf("want %v, got %v", v.Status.Used, get.Status.Used)
					}
				}
				return true, nil
			})
			if err != nil {
				klog.ErrorS(err, "Elastic Quota Test Failed")
				os.Exit(1)
			}
		})
	}
}
