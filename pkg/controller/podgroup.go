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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
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

	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
	schedlister "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// PodGroupController is a controller that process pod groups using provided Handler interface
type PodGroupController struct {
	eventRecorder   record.EventRecorder
	pgQueue         workqueue.RateLimitingInterface
	pgLister        schedlister.PodGroupLister
	podLister       corelister.PodLister
	pgListerSynced  cache.InformerSynced
	podListerSynced cache.InformerSynced
	pgClient        schedclientset.Interface
}

// NewPodGroupController returns a new *PodGroupController
func NewPodGroupController(client kubernetes.Interface,
	pgInformer schedinformer.PodGroupInformer,
	podInformer coreinformer.PodInformer,
	pgClient schedclientset.Interface) *PodGroupController {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: client.CoreV1().Events(v1.NamespaceAll)})

	ctrl := &PodGroupController{
		eventRecorder: broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "PodGroupController"}),
		pgQueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "PodGroup"),
	}

	pgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.pgAdded,
		UpdateFunc: ctrl.pgUpdated,
	})
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.podAdded,
		UpdateFunc: ctrl.podUpdated,
	})

	ctrl.pgLister = pgInformer.Lister()
	ctrl.podLister = podInformer.Lister()
	ctrl.pgListerSynced = pgInformer.Informer().HasSynced
	ctrl.podListerSynced = podInformer.Informer().HasSynced
	ctrl.pgClient = pgClient
	return ctrl
}

// Run starts listening on channel events
func (ctrl *PodGroupController) Run(workers int, stopCh <-chan struct{}) {
	defer ctrl.pgQueue.ShutDown()

	klog.InfoS("Starting Pod Group controller")
	defer klog.InfoS("Shutting Pod Group controller")

	if !cache.WaitForCacheSync(stopCh, ctrl.pgListerSynced, ctrl.podListerSynced) {
		klog.ErrorS(nil, "Cannot sync caches")
		return
	}
	klog.InfoS("Pod Group sync finished")
	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	<-stopCh
}

// pgAdded reacts to a PG creation
func (ctrl *PodGroupController) pgAdded(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	pg := obj.(*schedv1alpha1.PodGroup)
	if pg.Status.Phase == schedv1alpha1.PodGroupFinished || pg.Status.Phase == schedv1alpha1.PodGroupFailed {
		return
	}
	// If startScheduleTime - createTime > 2days, do not enqueue again because pod may have been GCed
	if pg.Status.Scheduled == pg.Spec.MinMember && pg.Status.Running == 0 &&
		pg.Status.ScheduleStartTime.Sub(pg.CreationTimestamp.Time) > 48*time.Hour {
		return
	}
	klog.InfoS("Enqueue podGroup ", "podGroup", key)
	ctrl.pgQueue.Add(key)
}

// pgUpdated reacts to a PG update
func (ctrl *PodGroupController) pgUpdated(old, new interface{}) {
	ctrl.pgAdded(new)
}

// podAdded reacts to a PG creation
func (ctrl *PodGroupController) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return
	}
	pg, err := ctrl.pgLister.PodGroups(pod.Namespace).Get(pgName)
	if err != nil {
		klog.ErrorS(err, "Error while adding pod")
		return
	}
	klog.V(5).InfoS("Add pod group when pod gets added", "podGroup", klog.KObj(pg), "pod", klog.KObj(pod))
	ctrl.pgAdded(pg)
}

// pgUpdated reacts to a PG update
func (ctrl *PodGroupController) podUpdated(old, new interface{}) {
	ctrl.podAdded(new)
}

func (ctrl *PodGroupController) worker() {
	for ctrl.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (ctrl *PodGroupController) processNextWorkItem() bool {
	keyObj, quit := ctrl.pgQueue.Get()
	if quit {
		return false
	}
	defer ctrl.pgQueue.Done(keyObj)

	key, ok := keyObj.(string)
	if !ok {
		ctrl.pgQueue.Forget(keyObj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", keyObj))
		return true
	}
	if err := ctrl.syncHandler(key); err != nil {
		runtime.HandleError(err)
		klog.ErrorS(err, "Error syncing pod group", "podGroup", key)
		return true
	}
	return true
}

// syncHandle syncs pod group and convert status
func (ctrl *PodGroupController) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	defer func() {
		if err != nil {
			ctrl.pgQueue.AddRateLimited(key)
			return
		}
	}()
	pg, err := ctrl.pgLister.PodGroups(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		klog.V(5).InfoS("Pod group has been deleted", "podGroup", key)
		return nil
	}
	if err != nil {
		klog.V(3).ErrorS(err, "Unable to retrieve pod group from store", "podGroup", key)
		return err
	}

	pgCopy := pg.DeepCopy()
	selector := labels.Set(map[string]string{schedv1alpha1.PodGroupLabel: pgCopy.Name}).AsSelector()
	pods, err := ctrl.podLister.List(selector)
	if err != nil {
		klog.ErrorS(err, "List pods for group failed", "podGroup", klog.KObj(pgCopy))
		return err
	}

	switch pgCopy.Status.Phase {
	case "":
		pgCopy.Status.Phase = schedv1alpha1.PodGroupPending
	case schedv1alpha1.PodGroupPending:
		if len(pods) >= int(pg.Spec.MinMember) {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupPreScheduling
			fillOccupiedObj(pg, pods[0])
		}
	default:
		var (
			running   int32 = 0
			succeeded int32 = 0
			failed    int32 = 0
		)
		if len(pods) != 0 {
			for _, pod := range pods {
				switch pod.Status.Phase {
				case v1.PodRunning:
					running++
				case v1.PodSucceeded:
					succeeded++
				case v1.PodFailed:
					failed++
				}
			}
		}
		pgCopy.Status.Failed = failed
		pgCopy.Status.Succeeded = succeeded
		pgCopy.Status.Running = running

		if len(pods) == 0 {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupPending
			break
		}

		if pgCopy.Status.Scheduled >= pgCopy.Spec.MinMember && pgCopy.Status.Phase == schedv1alpha1.PodGroupScheduling {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupScheduled
		}

		if pgCopy.Status.Succeeded+pgCopy.Status.Running >= pg.Spec.MinMember && pgCopy.Status.Phase == schedv1alpha1.PodGroupScheduled {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupRunning
		}
		// Final state of pod group
		if pgCopy.Status.Failed != 0 && pgCopy.Status.Failed+pgCopy.Status.Running+pgCopy.Status.Succeeded >= pg.Spec.
			MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupFailed
		}
		if pgCopy.Status.Succeeded >= pg.Spec.MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupFinished
		}
	}

	err = ctrl.patchPodGroup(pg, pgCopy)
	if err == nil {
		ctrl.pgQueue.Forget(pg)
	}
	return err
}

func (ctrl *PodGroupController) patchPodGroup(old, new *schedv1alpha1.PodGroup) error {
	if !reflect.DeepEqual(old, new) {
		patch, err := util.CreateMergePatch(old, new)
		if err != nil {
			return err
		}

		_, err = ctrl.pgClient.SchedulingV1alpha1().PodGroups(old.Namespace).Patch(context.TODO(), old.Name, types.MergePatchType,
			patch, metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func fillOccupiedObj(pg *schedv1alpha1.PodGroup, pod *v1.Pod) {
	var refs []string
	for _, ownerRef := range pod.OwnerReferences {
		refs = append(refs, fmt.Sprintf("%s/%s", pod.Namespace, ownerRef.Name))
	}
	if len(pg.Status.OccupiedBy) == 0 {
		return
	}
	if len(refs) != 0 {
		sort.Strings(refs)
		pg.Status.OccupiedBy = strings.Join(refs, ",")
	}
}
