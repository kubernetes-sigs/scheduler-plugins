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

	if p1 != p2 {
		return p1 > p2
	}
	qosResult := compQOS(pInfo1.Pod, pInfo2.Pod)
	if qosResult != 0 {
		return qosResult > 0
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

	// Define the precedence order of QoS classes using a map
	qosOrder := map[v1.PodQOSClass]int{
		v1.PodQOSBestEffort: 1,
		v1.PodQOSBurstable:  2,
		v1.PodQOSGuaranteed: 3,
	}

	if qosOrder[p1QOS] > qosOrder[p2QOS] {
		return 1
	} else if qosOrder[p1QOS] < qosOrder[p2QOS] {
		return -1
	}
	return 0
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &Sort{}, nil
}
