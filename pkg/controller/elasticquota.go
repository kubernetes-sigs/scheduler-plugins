/**
 * Author:        Sun Jichuan
 *
 * START DATE:    2021/7/4 5:22 PM
 *
 * CONTACT:       jichuan.sun@smartmore.com
 */

package controller

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	quota "k8s.io/apiserver/pkg/quota/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	coreinformer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	kubefeatures "k8s.io/kubernetes/pkg/features"

	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	schedlister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
)

// ElasticQuotaController is a controller that process elastic quota using provided Handler interface
type ElasticQuotaController struct {
	// eqClient is a clientSet for SchedulingV1alpha1 API group
	eqClient schedclientset.Interface

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
	kubeClientSet kubernetes.Interface,
	eqClient schedclientset.Interface,
	eqInformer schedinformer.ElasticQuotaInformer,
	podInformer coreinformer.PodInformer,
	recorder record.EventRecorder,
) *ElasticQuotaController {
	utilruntime.Must(schedv1alpha1.AddToScheme(scheme.Scheme))
	klog.V(5).Info("Creating event broadcaster")
	// set up elastic quota ctrl
	ctrl := &ElasticQuotaController{
		eqClient:        eqClient,
		eqLister:        eqInformer.Lister(),
		podLister:       podInformer.Lister(),
		eqListerSynced:  eqInformer.Informer().HasSynced,
		podListerSynced: podInformer.Informer().HasSynced,
		eqQueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ElasticQuota"),
		recorder:        recorder,
	}
	klog.V(5).Info("Setting up elastic quota event handlers")
	eqInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    ctrl.eqAdded,
			UpdateFunc: ctrl.eqUpdated,
			DeleteFunc: ctrl.eqDeleted,
		},
	)
	klog.V(5).Info("Setting up pod event handlers")
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
	klog.Info("Starting Elastic Quota control loop")
	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, ctrl.eqListerSynced, ctrl.podListerSynced); !ok {
		klog.Error("Cannot sync caches")
		return
	}
	klog.Info("Elastic Quota sync finished")
	klog.V(5).Infof("Starting %d workers to process elastic quota", workers)
	// Launch workers to process elastic quota resources
	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.run, time.Second, stopCh)
	}
	<-stopCh
	klog.V(5).Info("Shutting down elastic quota workers")
}

func (ctrl *ElasticQuotaController) run() {
	for ctrl.sync() {
	}
}

// sync deals with one key off the queue.  It returns false when it's time to quit.
func (ctrl *ElasticQuotaController) sync() bool {
	keyObj, quit := ctrl.eqQueue.Get()
	if quit {
		return false
	}
	err := func(obj interface{}) error {
		defer ctrl.eqQueue.Done(obj)
		key, ok := obj.(string)
		if !ok {
			ctrl.eqQueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", keyObj))
			return nil
		}
		if err := ctrl.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing elastic quota '%s': %s", key, err.Error())
		}
		ctrl.eqQueue.Forget(obj)
		klog.V(5).Infof("Successfully synced '%s'", key)
		return nil
	}(keyObj)
	if err != nil {
		runtime.HandleError(err)
		return true
	}
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
	if err != nil {
		// The elastic quota may no longer exist, in which case we stop processing.
		if apierrs.IsNotFound(err) {
			_, err = ctrl.eqClient.SchedulingV1alpha1().ElasticQuotas(namespace).Get(context.TODO(),
				name, metav1.GetOptions{})
			if err != nil && apierrs.IsNotFound(err) {
				// elastic quota was deleted in the meantime, ignore.
				klog.V(3).Infof("elastic quota has been %q deleted", name)
				return nil
			}
			return nil
		}
		runtime.HandleError(fmt.Errorf("failed to get elastic quota by: %s/%s", namespace, name))
		return err
	}

	klog.V(5).Infof("Try to process elastic quota: %#v", key)
	used, err := ctrl.computeNamespacedUsed(namespace, eq)
	if err != nil {
		return err
	}
	// create a usage object that is based on the elastic quota version that will handle updates
	// by default, we set used to the current status
	usage := eq.DeepCopy()
	usage.Status = schedv1alpha1.ElasticQuotaStatus{
		Used: used,
	}
	// Ignore this loop if the usage value has not changed
	if apiequality.Semantic.DeepEqual(usage.Status, eq.Status) {
		return nil
	}
	if _, err = ctrl.eqClient.SchedulingV1alpha1().ElasticQuotas(namespace).
		Update(context.TODO(), usage, metav1.UpdateOptions{}); err != nil {
		return err
	}
	ctrl.recorder.Event(eq, v1.EventTypeNormal, "Synced", "Elastic Quota synced successfully")
	return nil
}

func (ctrl *ElasticQuotaController) computeNamespacedUsed(namespace string, eq *schedv1alpha1.ElasticQuota) (v1.ResourceList, error) {
	used := v1.ResourceList{}
	pods, err := ctrl.podLister.Pods(namespace).List(labels.NewSelector())
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		// ignore pending pods
		if p.Status.Phase != v1.PodPending {
			used = quota.Add(used, computePodResourceRequest(p))
		}
	}
	// ensure set of used values match those that defined in min and max
	used = setUsed(eq, used)
	return used, nil
}

// eqAdded reacts to a ElasticQuota creation
func (ctrl *ElasticQuotaController) eqAdded(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	klog.V(5).Infof("Enqueue elastic quota key %v", key)
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
//   InitContainers
//     IC1:
//       CPU: 2
//       Memory: 1G
//     IC2:
//       CPU: 2
//       Memory: 3G
//   Containers
//     C1:
//       CPU: 2
//       Memory: 1G
//     C2:
//       CPU: 1
//       Memory: 1G
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
	if pod.Spec.Overhead != nil && utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodOverhead) {
		quota.Add(result, pod.Spec.Overhead)
	}
	// take max_resource for init_containers and containers
	return quota.Max(result, initRes)
}

// setUsed is takes the union of Quota.min and Quota.max
func setUsed(eq *schedv1alpha1.ElasticQuota, used v1.ResourceList) v1.ResourceList {
	minResources := quota.ResourceNames(eq.Spec.Min)
	minUsed := quota.Mask(used, minResources)
	maxResources := quota.ResourceNames(eq.Spec.Max)
	maxUsed := quota.Mask(used, maxResources)
	used = quota.Max(minUsed, maxUsed)
	return used
}
