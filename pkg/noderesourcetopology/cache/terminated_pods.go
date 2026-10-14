/*
Copyright The Kubernetes Authors.

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
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
)

// SetupTerminatedOwnPodsHandler registers a Delete handler on the pod informer for pods
// that were scheduled by this scheduler profile and request exclusive resources, aligning
// with kubelet Topology Manager decisions.
//
// On delete, it checks if the pod is found in the assumed resources map. If it is, it clears
// its assumed overlay and marks the node dirty while keeping it Fresh which will allow the
// Filter to execute for the node again.
// If the pod is not in the assumed resources map, it marks the node !Fresh until Resync+PFP flush
func SetupTerminatedOwnPodsHandler(lh logr.Logger, profileName string, podInformer k8scache.SharedInformer, cc *OverReserve) {
	onDelete := func(obj interface{}) {
		pod, ok := extractTerminatedPod(obj)
		if !ok {
			lh.V(3).Info("unsupported object type in terminated pods handler", "kind", fmt.Sprintf("%T", obj))
			return
		}
		if pod.Spec.NodeName == "" {
			return
		}
		if pod.Spec.SchedulerName != profileName {
			return
		}
		nrtResources := cc.nrtResNames.Get(pod.Spec.NodeName)
		if !resourcerequests.AreExclusiveForPod(pod, nrtResources) {
			return
		}
		lh := lh.WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, pod.Spec.NodeName)
		lh.V(4).Info("detected terminated own exclusive-resource pod")
		cc.RemoveAssumedPodOnTermination(pod.Spec.NodeName, pod)
	}

	podInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		DeleteFunc: onDelete,
	})
}

// extractTerminatedPod extracts a *corev1.Pod from either a direct object or a
// DeletedFinalStateUnknown tombstone that the informer emits on cache re-sync.
func extractTerminatedPod(obj interface{}) (*corev1.Pod, bool) {
	if pod, ok := obj.(*corev1.Pod); ok {
		return pod, true
	}
	if tombstone, ok := obj.(k8scache.DeletedFinalStateUnknown); ok {
		if pod, ok := tombstone.Obj.(*corev1.Pod); ok {
			return pod, true
		}
	}
	return nil, false
}
