/*
Copyright 2022 The Kubernetes Authors.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformer "k8s.io/client-go/informers/core/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	schedlister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// Constants applied by the AppGroup controller
const (
	timeLimitation = 48 * time.Hour
)

// AppGroupController : a controller that processes App groups using provided Handler interface
type AppGroupController struct {
	eventRecorder   record.EventRecorder
	agQueue         workqueue.RateLimitingInterface
	agLister        schedlister.AppGroupLister
	podLister       corelister.PodLister
	agListerSynced  cache.InformerSynced
	podListerSynced cache.InformerSynced
	agClient        schedclientset.Interface
}

// NewAppGroupController : returns a new *AppGroupController
func NewAppGroupController(client kubernetes.Interface,
	agInformer schedinformer.AppGroupInformer,
	podInformer coreinformer.PodInformer,
	agClient schedclientset.Interface) *AppGroupController {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: client.CoreV1().Events(v1.NamespaceAll)})

	ctrl := &AppGroupController{
		eventRecorder: broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "AppGroupController"}),
		agQueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AppGroup"),
	}

	klog.V(5).InfoS("Setting up AppGroup event handlers")
	agInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.agAdded,
		UpdateFunc: ctrl.agUpdated,
		DeleteFunc: ctrl.agDeleted,
	})

	klog.V(5).InfoS("Setting up Pod event handlers")
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.podAdded,
		UpdateFunc: ctrl.podUpdated,
		DeleteFunc: ctrl.podDeleted,
	})

	ctrl.agLister = agInformer.Lister()
	ctrl.podLister = podInformer.Lister()
	ctrl.agListerSynced = agInformer.Informer().HasSynced
	ctrl.podListerSynced = podInformer.Informer().HasSynced
	ctrl.agClient = agClient
	return ctrl
}

// Run : starts listening on channel events
func (ctrl *AppGroupController) Run(workers int, stopCh <-chan struct{}) {
	defer ctrl.agQueue.ShutDown()

	klog.InfoS("Starting App Group controller")
	defer klog.InfoS("Shutting App Group controller")

	if !cache.WaitForCacheSync(stopCh, ctrl.agListerSynced, ctrl.podListerSynced) {
		klog.Error("Cannot sync caches")
		return
	}
	klog.InfoS("App Group sync finished")
	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	<-stopCh
}

// agAdded : reacts to a AppGroup creation
func (ctrl *AppGroupController) agAdded(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	ag := obj.(*v1alpha1.AppGroup)

	// If startScheduleTime - createTime > 2days, do not enqueue again because pod may have been GCed
	if ag.Status.RunningWorkloads == 0 &&
		ag.Status.ScheduleStartTime.Sub(ag.CreationTimestamp.Time) > timeLimitation {
		return
	}

	klog.V(5).InfoS("Enqueue AppGroup ", "app group", key)
	ctrl.agQueue.Add(key)
}

// agUpdated : reacts to a AppGroup update
func (ctrl *AppGroupController) agUpdated(old, new interface{}) {
	ctrl.agAdded(new)
}

// agDeleted : reacts to a AppGroup deletion
func (ctrl *AppGroupController) agDeleted(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	klog.V(5).InfoS("Enqueue deleted app group key", "appGroup", key)
	ctrl.agQueue.AddRateLimited(key)
}

// podAdded : reacts to a pod creation
func (ctrl *AppGroupController) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)
	agName := util.GetPodAppGroupLabel(pod)
	if len(agName) == 0 {
		return
	}
	ag, err := ctrl.agLister.AppGroups(pod.Namespace).Get(agName)
	if err != nil {
		klog.ErrorS(err, "Error while adding pod")
		return
	}
	klog.V(5).InfoS("Add App group when pod gets added", "AppGroup", klog.KObj(ag), "pod", klog.KObj(pod))
	ctrl.agAdded(ag)
}

// podDeleted : reacts to a pod deletion
func (ctrl *AppGroupController) podDeleted(obj interface{}) {
	_, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	ctrl.podAdded(obj)
}

// pgUpdated : reacts to a pod update
func (ctrl *AppGroupController) podUpdated(old, new interface{}) {
	ctrl.podAdded(new)
}

func (ctrl *AppGroupController) worker() {
	for ctrl.processNextWorkItem() {
	}
}

// processNextWorkItem : deals with one key off the queue.  It returns false when it's time to quit.
func (ctrl *AppGroupController) processNextWorkItem() bool {
	keyObj, quit := ctrl.agQueue.Get()
	if quit {
		return false
	}
	defer ctrl.agQueue.Done(keyObj)

	key, ok := keyObj.(string)
	if !ok {
		ctrl.agQueue.Forget(keyObj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", keyObj))
		return true
	}
	if err := ctrl.syncHandler(key); err != nil {
		runtime.HandleError(err)
		klog.ErrorS(err, "Error syncing app group", "appGroup", key)
		return true
	}

	return true
}

// syncHandler : syncs AppGroup and converts the status
func (ctrl *AppGroupController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	defer func() {
		if err != nil {
			ctrl.agQueue.AddRateLimited(key)
			return
		}
	}()
	ag, err := ctrl.agLister.AppGroups(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		klog.V(5).InfoS("App group has been deleted", "appGroup", key)
		return nil
	}
	if err != nil {
		klog.V(3).ErrorS(err, "Unable to retrieve app group from store", "appGroup", key)
		return err
	}

	agCopy := ag.DeepCopy()
	selector := labels.Set(map[string]string{v1alpha1.AppGroupLabel: agCopy.Name}).AsSelector()

	pods, err := ctrl.podLister.List(selector)
	if err != nil {
		klog.ErrorS(err, "List pods for App group failed", "AppGroup", klog.KObj(agCopy))
		return err
	}
	// Update Status of AppGroup CRD
	// Running Workloads
	var numWorkloadsRunning int32 = 0
	if len(pods) != 0 {
		for _, pod := range pods {
			switch pod.Status.Phase {
			case v1.PodRunning:
				numWorkloadsRunning++
			}
		}
	}

	agCopy.Status.RunningWorkloads = numWorkloadsRunning
	klog.V(5).Info("RunningWorkloads: ", numWorkloadsRunning)

	if agCopy.Status.TopologyCalculationTime.IsZero() {
		klog.V(5).InfoS("Initial Calculation of Topology order...")
		agCopy.Status.TopologyOrder, err = calculateTopologyOrder(agCopy, agCopy.Spec.TopologySortingAlgorithm, agCopy.Spec.Workloads, err)
		if err != nil {
			klog.InfoS("Error Calculating Topology order, application reflects a DAG...", "appGroup", key)
			agCopy.Status.TopologyOrder = defaultTopologyOrder(agCopy.Spec.Workloads)
		}
		agCopy.Status.TopologyCalculationTime = metav1.Time{Time: time.Now()}

	} else if time.Now().Sub(ag.Status.TopologyCalculationTime.Time) > 24*time.Hour {
		klog.InfoS("Recalculation of Topology Order... Every 24 hours...")
		agCopy.Status.TopologyOrder, err = calculateTopologyOrder(agCopy, agCopy.Spec.TopologySortingAlgorithm, agCopy.Spec.Workloads, err)
		if err != nil {
			klog.InfoS("Error Calculating Topology order, application reflects a DAG...", "appGroup", key)
			agCopy.Status.TopologyOrder = defaultTopologyOrder(agCopy.Spec.Workloads)
		}
		agCopy.Status.TopologyCalculationTime = metav1.Time{Time: time.Now()}
	}
	klog.V(5).Info("ag to patch: ", agCopy)

	err = ctrl.patchAppGroup(ag, agCopy)
	if err == nil {
		ctrl.agQueue.Forget(ag)
	}
	return err
}

// patchAppGroup : patches the new info to the AppGroup
func (ctrl *AppGroupController) patchAppGroup(old, new *v1alpha1.AppGroup) error {
	if reflect.DeepEqual(old, new) {
		return nil
	}

	patch, err := util.CreateMergePatch(old, new)
	if err != nil {
		return err
	}

	_, err = ctrl.agClient.SchedulingV1alpha1().AppGroups(old.Namespace).Patch(context.TODO(), old.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// calculateTopologyOrder : calculates the correct sequence order for workload deployment based on the selected sorting algorithm
func calculateTopologyOrder(agCopy *v1alpha1.AppGroup, algorithm string, workloadList v1alpha1.AppGroupWorkloadList, err error) (v1alpha1.AppGroupTopologyList, error) {

	var order []string
	var topologyList v1alpha1.AppGroupTopologyList
	tree := map[string][]string{}

	for _, w := range workloadList {
		for _, dependency := range w.Dependencies {
			tree[w.Workload.Name] = append(tree[w.Workload.Name], dependency.Workload.Name)
		}
	}

	klog.V(5).Info("Service Dependency Tree: ", tree)

	// Calculate order based on the specified algorithm
	type sortImpl func(tree map[string][]string) ([]string, error)
	sortingAlgorithms := map[string]sortImpl{
		v1alpha1.AppGroupKahnSort:        util.KahnSort,
		v1alpha1.AppGroupTarjanSort:      util.TarjanSort,
		v1alpha1.AppGroupReverseKahn:     util.ReverseKahn,
		v1alpha1.AppGroupReverseTarjan:   util.ReverseTarjan,
		v1alpha1.AppGroupAlternateKahn:   util.AlternateKahn,
		v1alpha1.AppGroupAlternateTarjan: util.AlternateTarjan,
	}

	sortAlg, exists := sortingAlgorithms[algorithm]
	if !exists {
		klog.V(5).Infof("Sorting algorithm not identified: %v. Defaulting to %v", agCopy.Spec.TopologySortingAlgorithm, v1alpha1.AppGroupKahnSort)
		// Default: consider KahnSort algorithm
		sortAlg = util.KahnSort
	} else {
		klog.V(5).InfoS("Sorting algorithm identified", "algorithm", sortAlg)
	}
	// Get TopologyList
	order, err = sortAlg(tree)
	if err != nil {
		klog.ErrorS(err, "Sorting failed", "algorithm", sortAlg, "AppGroup", klog.KObj(agCopy))
		return topologyList, err
	}

	// Sort workload data by Selector
	sort.Sort(util.BySelector(agCopy.Spec.Workloads))

	for id, workloadName := range order {
		index := int32(id + 1)
		// Find workload data by name (Binary Search)
		w := util.FindWorkloadBySelector(agCopy.Spec.Workloads, workloadName)
		if w == nil {
			err := fmt.Errorf("Unable to find workload by name... ")
			klog.Error(err, "Binary search failed")
			return topologyList, err
		}

		topologyList = append(topologyList, v1alpha1.AppGroupTopologyInfo{
			Workload: *w,
			Index:    index,
		})
	}

	// Sort TopologyList by Selector
	klog.V(5).Infof("Sort Topology List by Workload.Selector... ")
	sort.Sort(util.ByWorkloadSelector(topologyList))

	klog.V(5).Info("topologyList: ", topologyList)
	return topologyList, nil
}

// defaultTopologyOrder : retrieves a default sequence order for workload deployment
func defaultTopologyOrder(workloadList v1alpha1.AppGroupWorkloadList) v1alpha1.AppGroupTopologyList {
	var topologyList v1alpha1.AppGroupTopologyList

	for i, w := range workloadList {
		topologyList = append(topologyList, v1alpha1.AppGroupTopologyInfo{
			Workload: w.Workload,
			Index:    int32(i + 1),
		})
	}

	// Sort TopologyList by Selector
	klog.V(5).Infof("Sort Topology List by Workload.Selector... ")
	sort.Sort(util.ByWorkloadSelector(topologyList))

	klog.V(5).Info("topologyList: ", topologyList)
	return topologyList
}
