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

package podprovider

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

type PodFilterFunc func(pod *corev1.Pod, logID string) bool

func NewFromHandle(handle framework.Handle, cacheConf *apiconfig.NodeResourceTopologyCache) (k8scache.SharedIndexInformer, podlisterv1.PodLister, PodFilterFunc) {
	dedicated := wantsDedicatedInformer(cacheConf)
	if !dedicated {
		podHandle := handle.SharedInformerFactory().Core().V1().Pods() // shortcut
		return podHandle.Informer(), podHandle.Lister(), IsPodRelevantShared
	}

	podInformer := coreinformers.NewFilteredPodInformer(handle.ClientSet(), metav1.NamespaceAll, 0, cache.Indexers{}, nil)
	podLister := podlisterv1.NewPodLister(podInformer.GetIndexer())

	klog.V(5).InfoS("Start custom pod informer")
	ctx := context.Background()
	go podInformer.Run(ctx.Done())

	klog.V(5).InfoS("Syncing custom pod informer")
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)
	klog.V(5).InfoS("Synced custom pod informer")

	return podInformer, podLister, IsPodRelevantDedicated
}

// IsPodRelevantAlways is meant to be used in test only
func IsPodRelevantAlways(pod *corev1.Pod, logID string) bool {
	return true
}

func IsPodRelevantShared(pod *corev1.Pod, logID string) bool {
	// we are interested only about nodes which consume resources
	return pod.Status.Phase == corev1.PodRunning
}

func IsPodRelevantDedicated(pod *corev1.Pod, logID string) bool {
	// Every other phase we're interested into (see https://github.com/kubernetes-sigs/scheduler-plugins/pull/599).
	// Note PodUnknown is deprecated and reportedly no longer set since 2015 (!!)
	if pod.Status.Phase == corev1.PodPending {
		// this is unexpected, so we're loud about it
		klog.V(2).InfoS("nrtcache: Listed pod in Pending phase, ignored", "logID", logID, "podUID", pod.UID)
		return false
	}
	if pod.Spec.NodeName == "" {
		// this is very unexpected, so we're louder about it
		klog.InfoS("nrtcache: Listed pod unbound, ignored", "logID", logID, "podUID", pod.UID)
		return false
	}
	return true
}

func wantsDedicatedInformer(cacheConf *apiconfig.NodeResourceTopologyCache) bool {
	if cacheConf == nil {
		return false
	}
	if cacheConf.InformerMode == nil {
		return false
	}
	infMode := *cacheConf.InformerMode
	return infMode == apiconfig.CacheInformerDedicated
}
