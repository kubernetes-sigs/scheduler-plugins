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
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	coreinformer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/util"

	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	schedlister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
)

// ElasticQuotaController is a controller that process elastic quota using provided Handler interface
type ElasticQuotaController struct {
	// schedClient is a clientSet for SchedulingV1alpha1 API group
	schedClient schedclientset.Interface

	eqLister schedlister.ElasticQuotaLister
	// podLister is lister for pod event and uses to compute namespaced resource used
	podLister       corelister.PodLister
	eqListerSynced  cache.InformerSynced
	podListerSynced cache.InformerSynced
	eqQueue         workqueue.RateLimitingInterface
	recorder        record.EventRecorder
}

// NewElasticQuotaController returns a new *ElasticQuotaController
func NewElasticQuotaController(
	kubeClient kubernetes.Interface,
	eqInformer schedinformer.ElasticQuotaInformer,
	podInformer coreinformer.PodInformer,
	schedClient schedclientset.Interface,
	newOpt ...func(ctrl *ElasticQuotaController),
) *ElasticQuotaController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events(v1.NamespaceAll)})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "ElasticQuotaController"})
	// set up elastic quota ctrl
	ctrl := &ElasticQuotaController{
		schedClient:     schedClient,
		eqLister:        eqInformer.Lister(),
		podLister:       podInformer.Lister(),
		eqListerSynced:  eqInformer.Informer().HasSynced,
		podListerSynced: podInformer.Informer().HasSynced,
		eqQueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ElasticQuota"),
		recorder:        recorder,
	}
	for _, f := range newOpt {
		f(ctrl)
	}
	klog.V(5).InfoS("Setting up elastic quota event handlers")
	eqInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    ctrl.eqAdded,
			UpdateFunc: ctrl.eqUpdated,
			DeleteFunc: ctrl.eqDeleted,
		},
	)
	klog.V(5).InfoS("Setting up pod event handlers")
	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    ctrl.podAdded,
			UpdateFunc: ctrl.podUpdated,
			DeleteFunc: ctrl.podDeleted,
		},
	)
	return ctrl
}

func (ctrl *ElasticQuotaController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer ctrl.eqQueue.ShutDown()
	// Start the informer factories to begin populating the informer caches
	klog.InfoS("Starting Elastic Quota control loop")
	// Wait for the caches to be synced before starting workers
	klog.InfoS("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, ctrl.eqListerSynced, ctrl.podListerSynced); !ok {
		klog.ErrorS(nil, "Cannot sync caches")
		os.Exit(1)
	}
	klog.InfoS("Elastic Quota sync finished")
	klog.V(5).InfoS("Starting workers to process elastic quota", "workers", workers)
	// Launch workers to process elastic quota resources
	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}
	<-stopCh
	klog.V(2).InfoS("Shutting down elastic quota workers")
}

// WithFakeRecorder will set a fake recorder.It is usually used for unit testing
func WithFakeRecorder(bufferSize int) func(ctrl *ElasticQuotaController) {
	return func(ctrl *ElasticQuotaController) {
		ctrl.recorder = record.NewFakeRecorder(bufferSize)
	}
}

func (ctrl *ElasticQuotaController) worker() {
	for ctrl.processNextWorkItem() {
	}
}

// sync deals with one key off the queue.  It returns false when it's time to quit.
func (ctrl *ElasticQuotaController) processNextWorkItem() bool {
	keyObj, quit := ctrl.eqQueue.Get()
	if quit {
		return false
	}
	defer ctrl.eqQueue.Done(keyObj)
	key, ok := keyObj.(string)
	if !ok {
		ctrl.eqQueue.Forget(keyObj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", keyObj))
		return true
	}
	if err := ctrl.syncHandler(key); err != nil {
		runtime.HandleError(err)
		klog.ErrorS(err, "Error syncing elastic quota", "elasticQuota", key)
		return true
	}
	ctrl.eqQueue.Forget(keyObj)
	klog.V(5).InfoS("Successfully synced elastic quota ", "elasticQuota", key)
	return true
}

// syncHandler syncs elastic quota and convert status.used
func (ctrl *ElasticQuotaController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	// Get the elastic quota resource with this namespace/name
	eq, err := ctrl.eqLister.ElasticQuotas(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		klog.V(5).InfoS("ElasticQuota has been deleted", "elasticQuota", key)
		return nil
	}
	if err != nil {
		klog.V(3).ErrorS(err, "Unable to retrieve elastic quota from store", "elasticQuota", key)
		return err
	}

	klog.V(5).InfoS("Try to process elastic quota", "elasticQuota", key)
	used, err := ctrl.computeElasticQuotaUsed(namespace, eq)
	if err != nil {
		return err
	}
	// create a usage object that is based on the elastic quota version that will handle updates
	// by default, we set used to the current status
	newEQ := eq.DeepCopy()
	newEQ.Status.Used = used
	// Ignore this loop if the usage value has not changed
	if apiequality.Semantic.DeepEqual(newEQ.Status, eq.Status) {
		return nil
	}
	patch, err := util.CreateMergePatch(eq, newEQ)
	if err != nil {
		return err
	}
	if _, err = ctrl.schedClient.SchedulingV1alpha1().ElasticQuotas(namespace).
		Patch(context.TODO(), eq.Name, types.MergePatchType,
			patch, metav1.PatchOptions{}, "status"); err != nil {
		return err
	}
	ctrl.recorder.Event(eq, v1.EventTypeNormal, "Synced", fmt.Sprintf("Elastic Quota %s synced successfully", key))
	return nil
}

func (ctrl *ElasticQuotaController) computeElasticQuotaUsed(namespace string, eq *schedv1alpha1.ElasticQuota) (v1.ResourceList, error) {
	used := newZeroUsed(eq)
	pods, err := ctrl.podLister.Pods(namespace).List(labels.NewSelector())
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		if p.Status.Phase == v1.PodRunning {
			used = quota.Add(used, computePodResourceRequest(p))
		}
	}
	return used, nil
}

// eqAdded reacts to a ElasticQuota creation
func (ctrl *ElasticQuotaController) eqAdded(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	klog.V(5).InfoS("Enqueue new elastic quota key", "elasticQuota", key)
	ctrl.eqQueue.AddRateLimited(key)
}

// eqUpdated reacts to a ElasticQuota update
func (ctrl *ElasticQuotaController) eqUpdated(oldObj, newObj interface{}) {
	ctrl.eqAdded(newObj)
}

// eqDeleted reacts to a ElasticQuota deletion
func (ctrl *ElasticQuotaController) eqDeleted(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	klog.V(5).InfoS("Enqueue deleted elastic quota key", "elasticQuota", key)
	ctrl.eqQueue.AddRateLimited(key)
}

// podAdded reacts to a pod creation
func (ctrl *ElasticQuotaController) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)
	list, err := ctrl.eqLister.ElasticQuotas(pod.Namespace).List(labels.Everything())
	if err != nil {
		runtime.HandleError(err)
		return
	}
	if len(list) == 0 {
		return
	}
	// todo(yuchen-sun) When elastic quota supports multiple instances in a namespace, modify this
	ctrl.eqAdded(list[0])
}

// podUpdated reacts to a pod update
func (ctrl *ElasticQuotaController) podUpdated(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)
	if newPod.ResourceVersion == oldPod.ResourceVersion {
		return
	}
	ctrl.podAdded(newObj)
}

// podDeleted reacts to a pod delete
func (ctrl *ElasticQuotaController) podDeleted(obj interface{}) {
	_, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	ctrl.podAdded(obj)
}

// computePodResourceRequest returns a v1.ResourceList that covers the largest
// width in each resource dimension. Because init-containers run sequentially, we collect
// the max in each dimension iteratively. In contrast, we sum the resource vectors for
// regular containers since they run simultaneously.
//
// If Pod Overhead is specified and the feature gate is set, the resources defined for Overhead
// are added to the calculated Resource request sum
//
// Example:
//
// Pod:
//
//	InitContainers
//	  IC1:
//	    CPU: 2
//	    Memory: 1G
//	  IC2:
//	    CPU: 2
//	    Memory: 3G
//	Containers
//	  C1:
//	    CPU: 2
//	    Memory: 1G
//	  C2:
//	    CPU: 1
//	    Memory: 1G
//
// Result: CPU: 3, Memory: 3G
func computePodResourceRequest(pod *v1.Pod) v1.ResourceList {
	result := v1.ResourceList{}
	for _, container := range pod.Spec.Containers {
		result = quota.Add(result, container.Resources.Requests)
	}
	initRes := v1.ResourceList{}
	// take max_resource for init_containers
	for _, container := range pod.Spec.InitContainers {
		initRes = quota.Max(initRes, container.Resources.Requests)
	}
	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil {
		quota.Add(result, pod.Spec.Overhead)
	}
	// take max_resource for init_containers and containers
	return quota.Max(result, initRes)
}

// newZeroUsed will return the zero value of the union of min and max
func newZeroUsed(eq *schedv1alpha1.ElasticQuota) v1.ResourceList {
	minResources := quota.ResourceNames(eq.Spec.Min)
	maxResources := quota.ResourceNames(eq.Spec.Max)
	res := v1.ResourceList{}
	for _, v := range minResources {
		res[v] = *resource.NewQuantity(0, resource.DecimalSI)
	}
	for _, v := range maxResources {
		res[v] = *resource.NewQuantity(0, resource.DecimalSI)
	}
	return res
}
