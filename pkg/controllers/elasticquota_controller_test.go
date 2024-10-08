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

package controllers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
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
					Used(testutil.MakeResourceList().CPU(3).Mem(3).Obj()).Obj(),
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
			controller, kClient := setUpEQ(ctx, t, c.elasticQuotas, c.pods)
			for _, pod := range c.pods {
				if _, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
					Namespace: pod.Namespace,
					Name:      pod.Name,
				}}); err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
			}

			for _, e := range c.elasticQuotas {
				if _, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
					Namespace: e.Namespace,
					Name:      e.Name,
				}}); err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
			}
			err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 1*time.Second, false, func(ctx context.Context) (done bool, err error) {
				for _, v := range c.want {
					eq := &v1alpha1.ElasticQuota{
						ObjectMeta: metav1.ObjectMeta{
							Name:      v.Name,
							Namespace: v.Namespace,
						},
					}
					err := kClient.Get(ctx, client.ObjectKeyFromObject(eq), eq)
					if err != nil {
						return false, err
					}
					if !quota.Equals(eq.Status.Used, v.Status.Used) {
						return false, fmt.Errorf("%v: want %v, got %v", c.name, v.Status.Used, eq.Status.Used)
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

func setUpEQ(ctx context.Context,
	t *testing.T,
	eqs []*v1alpha1.ElasticQuota,
	pods []*v1.Pod) (*ElasticQuotaReconciler, client.WithWatch) {
	s := scheme.Scheme
	utilruntime.Must(v1alpha1.AddToScheme(s))

	client := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.ElasticQuota{}).
		Build()
	for _, eq := range eqs {
		err := client.Create(ctx, eq)
		if errors.IsAlreadyExists(err) {
			err = client.Update(ctx, eq)
		}
		if err != nil {
			t.Fatal("setup controller", err)
		}
	}
	for _, pod := range pods {
		err := client.Create(ctx, pod)
		if errors.IsAlreadyExists(err) {
			err = client.Update(ctx, pod)
		}
		if err != nil {
			t.Fatal("setup controller", err)
		}
	}
	controller := &ElasticQuotaReconciler{
		Client:   client,
		Scheme:   s,
		recorder: record.NewFakeRecorder(3),
	}

	return controller, client
}
