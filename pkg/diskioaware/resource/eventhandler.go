/*
Copyright 2024 The Kubernetes Authors.

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

package resource

import (
	"context"
	"fmt"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	externalinformer "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/informers/externalversions"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/normalizer"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/utils"
)

type IOEventHandler struct {
	cache     Handle // resourceType -> handle
	nm        *normalizer.NormalizerManager
	vClient   versioned.Interface // versioned client for CRDs
	podLister corelisters.PodLister
	nsLister  corelisters.NamespaceLister
}

func NewIOEventHandler(cache Handle, h framework.Handle, pl corelisters.PodLister, nl corelisters.NamespaceLister, nm *normalizer.NormalizerManager) *IOEventHandler {
	return &IOEventHandler{
		cache:     cache,
		vClient:   IoiContext.VClient,
		podLister: pl,
		nsLister:  nl,
		nm:        nm,
	}
}

func (ps *IOEventHandler) BuildEvtHandler(ctx context.Context, podInformer cache.SharedIndexInformer, iof externalinformer.SharedInformerFactory) error {
	ddInformer := iof.Diskio().V1alpha1().NodeDiskDevices().Informer()
	diInformer := iof.Diskio().V1alpha1().NodeDiskIOStatses().Informer()
	dh := cache.ResourceEventHandlerFuncs{
		AddFunc:    ps.AddDiskDevice,
		DeleteFunc: ps.DeleteDiskDevice,
	}
	if _, err := ddInformer.AddEventHandler(dh); err != nil {
		return err
	}
	// NodeIOStatus event handler
	ih := cache.ResourceEventHandlerFuncs{
		UpdateFunc: ps.UpdateNodeDiskIOStats,
	}
	if _, err := diInformer.AddEventHandler(ih); err != nil {
		return err
	}
	iof.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), ddInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync resource NodeDiskDevice")
	}
	if !cache.WaitForCacheSync(ctx.Done(), diInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync resource NodeDiskIOStats")
	}
	fhandler := cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				klog.Errorf("cannot convert to *v1.Pod: %v", obj)
				return false
			}
			if IoiContext.InNamespaceWhiteList(pod.Namespace) {
				return false
			}
			if pod.Spec.NodeName == "" {
				return false
			}
			return true
		},
		Handler: cache.ResourceEventHandlerFuncs{
			DeleteFunc: ps.DeletePod,
		},
	}
	if _, err := podInformer.AddEventHandler(fhandler); err != nil {
		return err
	}
	go podInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync Pod")
	}
	return nil
}

func (ps *IOEventHandler) AddDiskDevice(obj interface{}) {
	ndd, ok := obj.(*v1alpha1.NodeDiskDevice)
	if !ok {
		klog.Errorf("[AddDiskDevice]cannot convert obj to *v1alpha1.NodeDiskDevice: %v", obj)
		return
	}
	node := ndd.Spec.NodeName
	ps.cache.(CacheHandle).AddCacheNodeInfo(node, ndd.Spec.Devices)
	// find reserved pods on the reported node
	podLists := []string{}
	ctx := context.Background()
	namespaces, err := ps.nsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("[AddDiskDevice]get namespaces error: %v", err)
	}
	IoiContext.Lock()
	defer IoiContext.Unlock()
	for _, ns := range namespaces {
		if IoiContext.InNamespaceWhiteList(ns.Name) {
			continue
		}
		pods, err := ps.podLister.Pods(ns.Name).List(labels.Everything())
		if err != nil {
			klog.Errorf("[AddDiskDevice]get pods error: %v", err)
		}
		for _, pod := range pods {
			if pod.Spec.NodeName == node {
				podLists = append(podLists, fmt.Sprintf("%s-%s", pod.Name, pod.UID))
				// use model to get request
				model, err := ps.cache.(CacheHandle).GetDiskNormalizeModel(node)
				if err != nil {
					klog.Errorf("[AddDiskDevice]get disk normalize model error: %v", err)
					continue
				}
				normalizeFunc, err := ps.nm.GetNormalizer(model)
				if err != nil {
					klog.Errorf("[AddDiskDevice]get normalizer error: %v", err)
					continue
				}
				key := pod.Annotations[common.DiskIOAnnotation]
				reqStr, err := normalizeFunc(key)
				if err != nil {
					klog.Errorf("[AddDiskDevice]normalize request error: %v", err)
					continue
				}
				req, err := utils.RequestStrToQuantity(reqStr)
				if err != nil {
					klog.Errorf("[AddDiskDevice]request string to quantity error: %v", err)
					continue
				}
				err = ps.cache.(CacheHandle).AddPod(pod, node, req)
				if err != nil {
					klog.Errorf("[AddDiskDevice]add pod error: %v", err)
					continue
				}
				IoiContext.SetPodRequests(string(pod.UID), req)
			}
		}
	}
	IoiContext.SetReservedPods(node, podLists)
	// create or update CR
	sts, err := utils.GetNodeIOStatus(ctx, ps.vClient, node)
	if err != nil {
		// CR not exist, create one
		err := utils.CreateNodeIOStatus(ctx, ps.vClient, node, podLists)
		if err != nil {
			klog.Errorf("[AddDiskDevice]create CR error: %v", err)
		}
	} else {
		// CR exist, check pod list and update it
		if utils.ComparePodList(sts.Spec.ReservedPods, podLists) && sts.Generation == *sts.Status.ObservedGeneration {
			// update cache
			err := ps.cache.(CacheHandle).UpdateCacheNodeStatus(node, sts.Status)
			if err != nil {
				klog.Errorf("[AddDiskDevice]update cache error: %v", err)
			}
		} else {
			// update CR
			err := utils.UpdateNodeIOStatus(ctx, ps.vClient, node, podLists)
			if err != nil {
				klog.Errorf("[AddDiskDevice]update CR error: %v", err)
			}
		}
	}
}

func (ps *IOEventHandler) DeleteDiskDevice(obj interface{}) {
	dd, ok := obj.(*v1alpha1.NodeDiskDevice)
	if !ok {
		klog.Errorf("[DeleteDiskDevice]cannot convert obj to NodeDiskDevice: %v", obj)
		return
	}
	err := ps.cache.(CacheHandle).DeleteCacheNodeInfo(dd.Spec.NodeName)
	if err != nil {
		klog.Errorf("[DeleteDiskDevice]fail to delete nodeinfo: %v", err.Error())
	}
	ps.cache.(CacheHandle).PrintCacheInfo()
	IoiContext.Lock()
	defer IoiContext.Unlock()
	IoiContext.RemoveNode(dd.Spec.NodeName)
}

func (ps *IOEventHandler) UpdateNodeDiskIOStats(oldObj, newObj interface{}) {
	old, ok := oldObj.(*v1alpha1.NodeDiskIOStats)
	if !ok {
		klog.Errorf("[UpdateNodeDiskIOStats]cannot convert to *v1alpha1.NodeDiskIOStats: %v", oldObj)
		return
	}
	new, ok := newObj.(*v1alpha1.NodeDiskIOStats)
	if !ok {
		klog.Errorf("[UpdateNodeDiskIOStats]cannot convert to *v1alpha1.NodeDiskIOStats: %v", newObj)
		return
	}
	if utils.HashObject(old.Status) == utils.HashObject(new.Status) || *new.Status.ObservedGeneration != new.Generation {
		return
	}
	err := ps.cache.(CacheHandle).UpdateCacheNodeStatus(new.Spec.NodeName, new.Status)
	if err != nil {
		klog.Error("[UpdateNodeDiskIOStats] error: ", err.Error())
	}
}

func (ps *IOEventHandler) DeletePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.Errorf("[DeletePod]cannot convert to *v1.Pod: %v", obj)
		return
	}
	err := ps.cache.(CacheHandle).RemovePod(pod, pod.Spec.NodeName)
	if err != nil {
		klog.Error("[DeletePod] err: ", err)
	}
	ps.cache.(CacheHandle).PrintCacheInfo()
	err = IoiContext.RemovePod(pod, pod.Spec.NodeName)
	if err != nil {
		klog.Errorf("[DeletePod] fail to remove pod in ReservedPod: %v", err)
	}
}
