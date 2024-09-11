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

package cache

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
)

// DiscardReserved is intended to solve similiar problem as Overreserve Cache,
// which is to minimize amount of incorrect scheduling decisions based on stale NRT data.
// Unfortunately Overreserve cache only works for single-numa-node Topology Manager policy.
// Dis tries to minimize amount of Admission Errors and non-optimal placement
// when NodeResourceTopologyMatch plugin is used to schedule PODs requesting resources from multiple NUMA domains.
// There are scenarios where using DiscardReserved won't mitigate drawbacks of using Passthrough cache.
// NRT update is expected once PostBind triggers, but there's no guarantee about when this will happen.
// In cases like:
// - NFD(or any other component that advertises NRT) can be nonfunctional
// - network can be slow
// - Pod being scheduled after PostBind trigger and before NRT update
// in those cases DiscardReserved cache will act same as Passthrough cache
type DiscardReserved struct {
	rMutex         sync.RWMutex
	reservationMap map[string]map[types.UID]bool // Key is NodeName, value is Pod UID : reserved status
	client         ctrlclient.Client
	lh             logr.Logger
}

func NewDiscardReserved(lh logr.Logger, client ctrlclient.Client) Interface {
	return &DiscardReserved{
		client:         client,
		reservationMap: make(map[string]map[types.UID]bool),
		lh:             lh,
	}
}

func (pt *DiscardReserved) GetCachedNRTCopy(ctx context.Context, nodeName string, _ *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, CachedNRTInfo) {
	pt.rMutex.RLock()
	defer pt.rMutex.RUnlock()
	if t, ok := pt.reservationMap[nodeName]; ok {
		if len(t) > 0 {
			return nil, CachedNRTInfo{}
		}
	}

	info := CachedNRTInfo{Fresh: true}
	nrt := &topologyv1alpha2.NodeResourceTopology{}
	if err := pt.client.Get(ctx, types.NamespacedName{Name: nodeName}, nrt); err != nil {
		return nil, info
	}
	return nrt, info
}

func (pt *DiscardReserved) NodeMaybeOverReserved(nodeName string, pod *corev1.Pod) {}
func (pt *DiscardReserved) NodeHasForeignPods(nodeName string, pod *corev1.Pod)    {}

func (pt *DiscardReserved) ReserveNodeResources(nodeName string, pod *corev1.Pod) {
	pt.lh.V(5).Info("NRT Reserve", logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)
	pt.rMutex.Lock()
	defer pt.rMutex.Unlock()

	if pt.reservationMap[nodeName] == nil {
		pt.reservationMap[nodeName] = make(map[types.UID]bool)
	}
	pt.reservationMap[nodeName][pod.GetUID()] = true
}

func (pt *DiscardReserved) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {
	pt.lh.V(5).Info("NRT Unreserve", logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)

	pt.removeReservationForNode(nodeName, pod)
}

// PostBind is invoked to cleanup reservationMap
func (pt *DiscardReserved) PostBind(nodeName string, pod *corev1.Pod) {
	pt.lh.V(5).Info("NRT PostBind", logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)

	pt.removeReservationForNode(nodeName, pod)
}

func (pt *DiscardReserved) removeReservationForNode(nodeName string, pod *corev1.Pod) {
	pt.rMutex.Lock()
	defer pt.rMutex.Unlock()

	delete(pt.reservationMap[nodeName], pod.GetUID())
}
