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

package cache

import (
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
)

// The nodeIndexer and the go client facilities are global objects, so we need this to be global as well.
// Each profile must register its name in order to support foreign pods detection. Foreign pods are pods
// which are scheduled to nodes without the caching machinery knowning.
// Note this is NOT lock-protected because the scheduling framework calls New() sequentially,
// and plugin instances are meant to register their name using SetupPodCacheHandlers inside their New()
var (
	schedProfileNames      = sets.Set[string]{}
	onlyExclusiveResources = false
)

// PodCacheHandlersOpts selects which pod-informer behaviors to enable.
// A single EventHandler is registered so Delete runs at most one callback.
type PodCacheHandlersOpts struct {
	ForeignPodsDetect       bool
	TerminatedOwnPodsDetect bool
}

// SetupPodCacheHandlers registers a single pod informer handler for foreign-pod detection
// and/or same-profile terminated exclusive-resource pods.
//
// Add/Update run only when ForeignPodsDetect is set.
// Delete branches: foreign first (other scheduler), else terminated own-profile
// exclusive pods when TerminatedOwnPodsDetect is set. The two cases are mutually exclusive
// by SchedulerName for a given profile.
func SetupPodCacheHandlers(lh logr.Logger, profileName string, podInformer k8scache.SharedInformer, cc *OverReserve, opts PodCacheHandlersOpts) {
	if !opts.ForeignPodsDetect && !opts.TerminatedOwnPodsDetect {
		return
	}

	onForeign := func(obj interface{}) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			lh.V(3).Info("unsupported object", "kind", fmt.Sprintf("%T", obj))
			return
		}
		handleForeignPod(lh, cc, pod)
	}

	onDelete := func(obj interface{}) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			lh.V(3).Info("unsupported object", "kind", fmt.Sprintf("%T", obj))
			return
		}
		if pod.Spec.NodeName == "" {
			return
		}

		if opts.ForeignPodsDetect && handleForeignPod(lh, cc, pod) {
			return
		}
		if opts.TerminatedOwnPodsDetect {
			handleTerminatedOwnPod(lh, profileName, cc, pod)
		}
	}

	handlers := k8scache.ResourceEventHandlerFuncs{
		DeleteFunc: onDelete,
	}
	if opts.ForeignPodsDetect {
		handlers.AddFunc = onForeign
		handlers.UpdateFunc = func(oldObj, newObj interface{}) {
			onForeign(newObj)
		}
	}
	podInformer.AddEventHandler(handlers)
}

// handleForeignPod marks the node if pod is foreign. Returns true if the pod was handled as foreign.
func handleForeignPod(lh logr.Logger, cc *OverReserve, pod *corev1.Pod) bool {
	nrtResources := cc.nrtResNames.Get(pod.Spec.NodeName)
	if !IsForeignPod(pod, nrtResources) {
		return false
	}
	cc.NodeHasForeignPods(pod.Spec.NodeName, pod)
	lh.V(6).Info("detected foreign pods", logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, pod.Spec.NodeName)
	return true
}

// handleTerminatedOwnPod restores assumed overlay / marks WithTerminatedPods for a deleted
// same-profile exclusive-resource pod.
func handleTerminatedOwnPod(lh logr.Logger, profileName string, cc *OverReserve, pod *corev1.Pod) {
	if pod.Spec.SchedulerName != profileName {
		return
	}
	nrtResources := cc.nrtResNames.Get(pod.Spec.NodeName)
	if !resourcerequests.AreExclusiveForPod(pod, nrtResources) {
		return
	}
	lh = lh.WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, pod.Spec.NodeName)
	lh.V(4).Info("detected terminated own exclusive-resource pod")
	cc.RemoveAssumedPodOnTermination(pod.Spec.NodeName, pod)
}

func TrackOnlyForeignPodsWithExclusiveResources() {
	onlyExclusiveResources = true
}

func TrackAllForeignPods() {
	onlyExclusiveResources = false
}

func RegisterSchedulerProfileName(lh logr.Logger, schedProfileName string) {
	lh.Info("setting up detection", "profile", schedProfileName)
	schedProfileNames.Insert(schedProfileName)

	lh.V(5).Info("registered scheduler profiles", "names", schedProfileNames.UnsortedList())
}

func IsForeignPod(pod *corev1.Pod, nrtResources sets.Set[corev1.ResourceName]) bool {
	if pod.Spec.NodeName == "" {
		// nothing to do yet
		return false
	}
	if schedProfileNames.Has(pod.Spec.SchedulerName) {
		// nothing to do here - we know already about this pod
		return false
	}
	if !onlyExclusiveResources {
		return true
	}
	return resourcerequests.AreExclusiveForPod(pod, nrtResources)
}

// for testing only; NOT thread safe
func CleanRegisteredSchedulerProfileNames() {
	schedProfileNames = sets.Set[string]{}
}
