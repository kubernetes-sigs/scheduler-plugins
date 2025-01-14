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

package qos

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Name is the name of the plugin used in the plugin registry and configurations.
const Name = "QOSSort"

// Sort is a plugin that implements QoS class based sorting.
type Sort struct{}

var _ framework.QueueSortPlugin = &Sort{}

// Name returns name of the plugin.
func (pl *Sort) Name() string {
	return Name
}

// Less is the function used by the activeQ heap algorithm to sort pods.
// It sorts pods based on their priorities. When the priorities are equal, it uses
// the Pod QoS classes to break the tie. If both the priority and QoS class are equal,
// it uses PodQueueInfo.timestamp to determine the order.
func (*Sort) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	p1 := corev1helpers.PodPriority(pInfo1.Pod)
	p2 := corev1helpers.PodPriority(pInfo2.Pod)

	klog.V(4).Infof("Comparing Pod %s (priority: %d) with Pod %s (priority: %d)", pInfo1.Pod.Name, p1, pInfo2.Pod.Name, p2)

	if p1 != p2 {
		if p1 > p2 {
			klog.Infof("Pod %s has higher priority than Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
		} else {
			klog.Infof("Pod %s has lower priority than Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
		}
		return p1 > p2
	}

	klog.V(4).Infof("Pod %s and Pod %s have the same priority", pInfo1.Pod.Name, pInfo2.Pod.Name)

	qosResult := compQOS(pInfo1.Pod, pInfo2.Pod)
	if qosResult != 0 {
		if qosResult > 0 {
			klog.Infof("Pod %s has higher QoS than Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
		} else {
			klog.Infof("Pod %s has lower QoS than Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
		}
		return qosResult > 0
	}

	klog.Infof("Pod %s and Pod %s have the same QoS", pInfo1.Pod.Name, pInfo2.Pod.Name)

	if pInfo1.Timestamp.Before(pInfo2.Timestamp) {
		klog.Infof("Pod %s is older than Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
	} else {
		klog.Infof("Pod %s is newer or equal in age to Pod %s", pInfo1.Pod.Name, pInfo2.Pod.Name)
	}

	return pInfo1.Timestamp.Before(pInfo2.Timestamp)
}

// compQOS compares the QoS classes of two Pods and returns:
//
//	 1 if p1 has a higher precedence QoS class than p2,
//	-1 if p2 has a higher precedence QoS class than p1,
//	 0 if both have the same QoS class.
func compQOS(p1, p2 *v1.Pod) int {
	p1QOS, p2QOS := v1qos.GetPodQOS(p1), v1qos.GetPodQOS(p2)

	klog.Infof("Pod %s has QoS class %s", p1.Name, p1QOS)
	klog.Infof("Pod %s has QoS class %s", p2.Name, p2QOS)

	// Define the precedence order of QoS classes using a map
	qosOrder := map[v1.PodQOSClass]int{
		v1.PodQOSBestEffort: 1,
		v1.PodQOSBurstable:  2,
		v1.PodQOSGuaranteed: 3,
	}

	klog.Infof("Comparing QoS for Pod %s (%s) and Pod %s (%s)", p1.Name, p1QOS, p2.Name, p2QOS)
	klog.Infof("QoS order for Pod %s is %d, for Pod %s is %d", p1.Name, qosOrder[p1QOS], p2.Name, qosOrder[p2QOS])

	// Function to log resource requests and limits for a pod
	logResources := func(pod *v1.Pod, name string) {
		for _, container := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
			klog.Infof("Container %s in Pod %s:", container.Name, name)
			if len(container.Resources.Requests) > 0 {
				klog.Infof("  Requests:")
				for resourceName, quantity := range container.Resources.Requests {
					klog.Infof("    %s: %s", resourceName, quantity.String())
				}
			} else {
				klog.Infof("  No resource requests defined.")
			}

			if len(container.Resources.Limits) > 0 {
				klog.Infof("  Limits:")
				for resourceName, quantity := range container.Resources.Limits {
					klog.Infof("    %s: %s", resourceName, quantity.String())
				}
			} else {
				klog.Infof("  No resource limits defined.")
			}
		}
	}

	// Log resources for both pods
	logResources(p1, p1.Name)
	logResources(p2, p2.Name)
	
	if qosOrder[p1QOS] > qosOrder[p2QOS] {
		klog.Infof("Pod %s has higher QoS than Pod %s", p1.Name, p2.Name)
		return 1
	} else if qosOrder[p1QOS] < qosOrder[p2QOS] {
		klog.Infof("Pod %s has lower QoS than Pod %s", p1.Name, p2.Name)
		return -1
	}
	klog.Infof("Pod %s and Pod %s have the same QoS", p1.Name, p2.Name)
	return 0
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &Sort{}, nil
}
