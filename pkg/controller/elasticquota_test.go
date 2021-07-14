/**
 * Author:        Sun Jichuan
 *
 * START DATE:    2021/7/8 1:05 PM
 *
 * CONTACT:       jichuan.sun@smartmore.com
 */

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"

	"k8s.io/klog/v2"

	quota "k8s.io/apiserver/pkg/quota/v1"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/controller"
	pgfake "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"

	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"k8s.io/apimachinery/pkg/api/resource"

	v1 "k8s.io/api/core/v1"
)

func TestElasticQuotaController_Run(t *testing.T) {
	ctx := context.TODO()
	cases := []struct {
		name     string
		eqName   string
		min      v1.ResourceList
		max      v1.ResourceList
		nowUsed  v1.ResourceList
		pods     []*v1.Pod
		nextUsed v1.ResourceList
	}{
		{
			name:    "no init containers pod",
			eqName:  "eq1",
			min:     makeResourceList(3, 5, 0),
			max:     makeResourceList(5, 15, 1),
			nowUsed: makeResourceList(0, 0, 0),
			pods: []*v1.Pod{
				makePod("pod1", v1.PodRunning).container(makeResourceList(1, 2, 1)).obj(),
				makePod("pod2", v1.PodPending).container(makeResourceList(1, 2, 0)).obj(),
			},
			nextUsed: makeResourceList(1, 2, 1),
		},
		{
			name:    "have init containers pod",
			eqName:  "eq2",
			min:     makeResourceList(3, 5, 0),
			max:     makeResourceList(5, 15, 0),
			nowUsed: makeResourceList(1, 3, 0),
			pods: []*v1.Pod{
				// cpu:2,mem: 4
				makePod("pod1", v1.PodRunning).
					container(makeResourceList(1, 2, 1)).
					container(makeResourceList(1, 2, 0)).obj(),
				// cpu: 3, men: 3
				makePod("pod2", v1.PodRunning).
					initContainerRequest(makeResourceList(2, 1, 0)).
					initContainerRequest(makeResourceList(2, 3, 0)).
					container(makeResourceList(2, 1, 0)).
					container(makeResourceList(1, 1, 0)).obj(),
			},
			nextUsed: makeResourceList(5, 7, 0),
		},
		{
			name:    "update pods",
			eqName:  "eq3",
			min:     makeResourceList(3, 5, 0),
			max:     makeResourceList(5, 15, 0),
			nowUsed: makeResourceList(2, 4, 1),
			pods: []*v1.Pod{
				// cpu:2,mem: 4
				makePod("pod1", v1.PodRunning).
					container(makeResourceList(1, 2, 1)).
					container(makeResourceList(1, 2, 0)).obj(),
				// cpu: 3, men: 3
				makePod("pod1", v1.PodPending).
					initContainerRequest(makeResourceList(2, 1, 0)).
					initContainerRequest(makeResourceList(2, 3, 0)).
					container(makeResourceList(2, 1, 0)).
					container(makeResourceList(1, 1, 0)).obj(),
			},
			nextUsed: makeResourceList(0, 0, 0),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			eq := makeEQ(c.eqName, c.min, c.max, c.nowUsed)
			eqClient := pgfake.NewSimpleClientset(eq)
			informerFactory := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
			pgInformerFactory := schedinformer.NewSharedInformerFactory(eqClient, controller.NoResyncPeriodFunc())
			podInformer := informerFactory.Core().V1().Pods()
			eqInformer := pgInformerFactory.Scheduling().V1alpha1().ElasticQuotas()
			recorder := record.NewFakeRecorder(2)
			ctrl := NewElasticQuotaController(kubeClient, eqClient, eqInformer, podInformer, recorder)

			pgInformerFactory.Start(ctx.Done())
			informerFactory.Start(ctx.Done())
			// 0 means not set
			for _, p := range c.pods {
				kubeClient.Tracker().Add(p)
				kubeClient.CoreV1().Pods(p.Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
			}

			go ctrl.Run(1, ctx.Done())
			err := wait.Poll(200*time.Millisecond, 1*time.Second, func() (done bool, err error) {
				get, err := eqClient.SchedulingV1alpha1().ElasticQuotas("default").Get(ctx, c.eqName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if !quota.Equals(get.Status.Used, c.nextUsed) {
					return false, fmt.Errorf("want %v, got %v", c.nextUsed, get.Status.Used)
				}
				return true, nil
			})
			if err != nil {
				klog.Fatal(err)
			}
		})
	}
}

func makeResourceList(cpu, mem, gpu int64) v1.ResourceList {
	res := v1.ResourceList{}
	if cpu != 0 {
		res[v1.ResourceCPU] = *resource.NewQuantity(cpu, resource.DecimalSI)
	}
	if mem != 0 {
		res[v1.ResourceMemory] = *resource.NewQuantity(mem, resource.DecimalSI)
	}
	if gpu != 0 {
		res["nvidia.com/gpu"] = *resource.NewQuantity(gpu, resource.DecimalSI)
	}
	return res
}

type podWrapper struct{ *v1.Pod }

func makePod(name string, phase v1.PodPhase) *podWrapper {
	pod := st.MakePod().Namespace("default").Name(name).Obj()
	pod.Status.Phase = phase
	return &podWrapper{pod}
}

func (p *podWrapper) container(request v1.ResourceList) *podWrapper {
	p.Pod.Spec.Containers = append(p.Pod.Spec.Containers, v1.Container{
		Name:  fmt.Sprintf("con%d", len(p.Pod.Spec.Containers)),
		Image: "image",
		Resources: v1.ResourceRequirements{
			Requests: request,
		},
	})
	return p
}

func (p *podWrapper) initContainerRequest(request v1.ResourceList) *podWrapper {
	p.Pod.Spec.InitContainers = append(p.Pod.Spec.InitContainers, v1.Container{
		Name:  fmt.Sprintf("con%d", len(p.Pod.Spec.Containers)),
		Image: "image",
		Resources: v1.ResourceRequirements{
			Requests: request,
		},
	})
	return p
}

func (p *podWrapper) obj() *v1.Pod {
	return p.Pod
}

func makeEQ(name string, min, max, used v1.ResourceList) *v1alpha1.ElasticQuota {
	return &v1alpha1.ElasticQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1alpha1.ElasticQuotaSpec{
			Min: min,
			Max: max,
		},
		Status: v1alpha1.ElasticQuotaStatus{
			Used: used,
		},
	}
}
