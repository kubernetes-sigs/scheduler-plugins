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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

var lowPriority, midPriority, highPriority = int32(0), int32(100), int32(1000)

var signalHandler = ctrl.SetupSignalHandler()

// podScheduled returns true if a node is assigned to the given pod.
func podScheduled(c clientset.Interface, podNamespace, podName string) bool {
	pod, err := c.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		// This could be a connection error so we want to retry.
		klog.ErrorS(err, "Failed to get pod", "pod", klog.KRef(podNamespace, podName))
		return false
	}
	return pod.Spec.NodeName != ""
}

type resourceWrapper struct{ v1.ResourceList }

func MakeResourceList() *resourceWrapper {
	return &resourceWrapper{v1.ResourceList{}}
}

func (r *resourceWrapper) CPU(val int64) *resourceWrapper {
	r.ResourceList[v1.ResourceCPU] = *resource.NewQuantity(val, resource.DecimalSI)
	return r
}

func (r *resourceWrapper) Mem(val int64) *resourceWrapper {
	r.ResourceList[v1.ResourceMemory] = *resource.NewQuantity(val, resource.DecimalSI)
	return r
}

func (r *resourceWrapper) GPU(val int64) *resourceWrapper {
	r.ResourceList["nvidia.com/gpu"] = *resource.NewQuantity(val, resource.DecimalSI)
	return r
}

func (r *resourceWrapper) Obj() v1.ResourceList {
	return r.ResourceList
}

type podWrapper struct{ *v1.Pod }

func MakePod(namespace, name string) *podWrapper {
	pod := st.MakePod().Namespace(namespace).Name(name).Obj()

	return &podWrapper{pod}
}

func (p *podWrapper) Phase(phase v1.PodPhase) *podWrapper {
	p.Pod.Status.Phase = phase
	return p
}

func (p *podWrapper) Container(request v1.ResourceList) *podWrapper {
	p.Pod.Spec.Containers = append(p.Pod.Spec.Containers, v1.Container{
		Name:  fmt.Sprintf("con%d", len(p.Pod.Spec.Containers)),
		Image: "image",
		Resources: v1.ResourceRequirements{
			Requests: request,
		},
	})
	return p
}

func (p *podWrapper) InitContainerRequest(request v1.ResourceList) *podWrapper {
	p.Pod.Spec.InitContainers = append(p.Pod.Spec.InitContainers, v1.Container{
		Name:  fmt.Sprintf("con%d", len(p.Pod.Spec.Containers)),
		Image: "image",
		Resources: v1.ResourceRequirements{
			Requests: request,
		},
	})
	return p
}

func (p *podWrapper) Node(name string) *podWrapper {
	p.Pod.Spec.NodeName = name
	return p
}

func (p *podWrapper) Obj() *v1.Pod {
	return p.Pod
}

type eqWrapper struct{ *v1alpha1.ElasticQuota }

func MakeEQ(namespace, name string) *eqWrapper {
	eq := &v1alpha1.ElasticQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return &eqWrapper{eq}
}

func (e *eqWrapper) Min(min v1.ResourceList) *eqWrapper {
	e.ElasticQuota.Spec.Min = min
	return e
}

func (e *eqWrapper) Max(max v1.ResourceList) *eqWrapper {
	e.ElasticQuota.Spec.Max = max
	return e
}

func (e *eqWrapper) Used(used v1.ResourceList) *eqWrapper {
	e.ElasticQuota.Status.Used = used
	return e
}

func (e *eqWrapper) Obj() *v1alpha1.ElasticQuota {
	return e.ElasticQuota
}

type testContext struct {
	ClientSet          clientset.Interface
	KubeConfig         *restclient.Config
	InformerFactory    informers.SharedInformerFactory
	DynInformerFactory dynamicinformer.DynamicSharedInformerFactory
	Scheduler          *scheduler.Scheduler
	Ctx                context.Context
	CancelFn           context.CancelFunc
}

func initTestSchedulerWithOptions(t *testing.T, testCtx *testContext, opts ...scheduler.Option) *testContext {
	testCtx.InformerFactory = scheduler.NewInformerFactory(testCtx.ClientSet, 0)
	if testCtx.KubeConfig != nil {
		dynClient := dynamic.NewForConfigOrDie(testCtx.KubeConfig)
		testCtx.DynInformerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynClient, 0, v1.NamespaceAll, nil)
	}

	var err error
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: testCtx.ClientSet.EventsV1(),
	})

	opts = append(opts, scheduler.WithKubeConfig(testCtx.KubeConfig))
	testCtx.Scheduler, err = scheduler.New(
		testCtx.Ctx,
		testCtx.ClientSet,
		testCtx.InformerFactory,
		testCtx.DynInformerFactory,
		profile.NewRecorderFactory(eventBroadcaster),
		opts...,
	)

	if err != nil {
		t.Fatalf("Couldn't create scheduler: %v", err)
	}

	eventBroadcaster.StartRecordingToSink(testCtx.Ctx.Done())

	return testCtx
}

func syncInformerFactory(testCtx *testContext) {
	testCtx.InformerFactory.Start(testCtx.Ctx.Done())
	if testCtx.DynInformerFactory != nil {
		testCtx.DynInformerFactory.Start(testCtx.Ctx.Done())
	}
	testCtx.InformerFactory.WaitForCacheSync(testCtx.Ctx.Done())
	if testCtx.DynInformerFactory != nil {
		testCtx.DynInformerFactory.WaitForCacheSync(testCtx.Ctx.Done())
	}
}

func cleanupPods(t *testing.T, testCtx *testContext, pods []*v1.Pod) {
	var zero int64 = 0

	for _, p := range pods {
		err := testCtx.ClientSet.CoreV1().Pods(p.Namespace).Delete(testCtx.Ctx, p.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("error while deleting pod %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	for _, p := range pods {
		if err := wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond, wait.ForeverTestTimeout, false,
			podDeleted(testCtx.ClientSet, p.Namespace, p.Name)); err != nil {
			t.Errorf("error while waiting for pod  %s/%s to get deleted: %v", p.Namespace, p.Name, err)
		}
	}
}

func podDeleted(c clientset.Interface, podNamespace, podName string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		_, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	}
}

func cleanupTest(t *testing.T, testCtx *testContext) {
	// cleanup nodes
	if err := testCtx.ClientSet.CoreV1().Nodes().DeleteCollection(testCtx.Ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		t.Fatalf("Failed to clean up created nodes: %v", err)
	}
	// kill the scheduler
	testCtx.CancelFn()
}

func createNamespace(t *testing.T, testCtx *testContext, ns string) {
	_, err := testCtx.ClientSet.CoreV1().Namespaces().Create(testCtx.Ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Failed to create integration test namespace %s: %v", ns, err)
	}
}

func createAppGroups(ctx context.Context, client ctrlclient.Client, appGroups []*agv1alpha1.AppGroup) error {
	for _, ag := range appGroups {
		err := client.Create(ctx, ag.DeepCopy())
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupAppGroups(ctx context.Context, client ctrlclient.Client, appGroups []*agv1alpha1.AppGroup) {
	for _, ag := range appGroups {
		client.Delete(ctx, ag)
	}
}

func createNetworkTopologies(ctx context.Context, client ctrlclient.Client, networkTopologies []*ntv1alpha1.NetworkTopology) error {
	for _, nt := range networkTopologies {
		err := client.Create(ctx, nt.DeepCopy())
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupNetworkTopologies(ctx context.Context, client ctrlclient.Client, networkTopologies []*ntv1alpha1.NetworkTopology) {
	for _, nt := range networkTopologies {
		client.Delete(ctx, nt)
	}
}

// AppGroup wrapper
type agWrapper struct{ *agv1alpha1.AppGroup }

func MakeAppGroup(namespace, name string) *agWrapper {
	ag := &agv1alpha1.AppGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return &agWrapper{ag}
}

func (a *agWrapper) Name(name string) *agWrapper {
	a.AppGroup.Name = name
	return a
}

func (a *agWrapper) Namespace(namespace string) *agWrapper {
	a.AppGroup.Namespace = namespace
	return a
}

func (a *agWrapper) Spec(spec agv1alpha1.AppGroupSpec) *agWrapper {
	a.AppGroup.Spec = spec
	return a
}

func (a *agWrapper) Status(status agv1alpha1.AppGroupStatus) *agWrapper {
	a.AppGroup.Status = status
	return a
}

func (a *agWrapper) Obj() *agv1alpha1.AppGroup {
	return a.AppGroup
}

// NetworkTopology wrapper
type ntWrapper struct{ *ntv1alpha1.NetworkTopology }

func MakeNetworkTopology(namespace, name string) *ntWrapper {
	nt := &ntv1alpha1.NetworkTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return &ntWrapper{nt}
}

func (nt *ntWrapper) Name(name string) *ntWrapper {
	nt.NetworkTopology.Name = name
	return nt
}

func (nt *ntWrapper) Namespace(namespace string) *ntWrapper {
	nt.NetworkTopology.Namespace = namespace
	return nt
}

func (nt *ntWrapper) Spec(spec ntv1alpha1.NetworkTopologySpec) *ntWrapper {
	nt.NetworkTopology.Spec = spec
	return nt
}

func (nt *ntWrapper) Status(status ntv1alpha1.NetworkTopologyStatus) *ntWrapper {
	nt.NetworkTopology.Status = status
	return nt
}

func (nt *ntWrapper) Obj() *ntv1alpha1.NetworkTopology {
	return nt.NetworkTopology
}
