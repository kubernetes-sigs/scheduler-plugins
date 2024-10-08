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
// and plugin instances are meant to register their name using SetupForeignPodsDetector inside their New()
var (
	schedProfileNames      = sets.String{}
	onlyExclusiveResources = false
)

func SetupForeignPodsDetector(lh logr.Logger, schedProfileName string, podInformer k8scache.SharedInformer, cc Interface) {
	foreignCache := func(obj interface{}) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			lh.V(3).Info("unsupported object", "kind", fmt.Sprintf("%T", obj))
			return
		}
		if !IsForeignPod(pod) {
			return
		}

		cc.NodeHasForeignPods(pod.Spec.NodeName, pod)
		lh.V(6).Info("detected foreign pods", logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, pod.Spec.NodeName)
	}

	podInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: foreignCache,
		UpdateFunc: func(oldObj, newObj interface{}) {
			foreignCache(newObj)
		},
		DeleteFunc: foreignCache,
	})
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

	lh.V(5).Info("registered scheduler profiles", "names", schedProfileNames.List())
}

func IsForeignPod(pod *corev1.Pod) bool {
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
	return resourcerequests.AreExclusiveForPod(pod)
}

// for testing only; NOT thread safe
func CleanRegisteredSchedulerProfileNames() {
	schedProfileNames = sets.String{}
}
