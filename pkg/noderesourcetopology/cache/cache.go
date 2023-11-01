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
	"context"

	corev1 "k8s.io/api/core/v1"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

type Interface interface {
	// GetCachedNRTCopy retrieves a NRT copy from cache, and then deducts over-reserved resources if necessary.
	// It will be used as the source of truth across the Pod's scheduling cycle.
	// Over-reserved resources are the resources consumed by pods scheduled to that node after the last update
	// of NRT pertaining to the same node, pessimistically overallocated on ALL the NUMA zones of the node.
	// The pod argument is used only for logging purposes.
	// Returns a boolean to signal the caller if the NRT data is clean. If false, then the node has foreign
	// Pods detected - so it should be ignored or handled differently by the caller.
	GetCachedNRTCopy(ctx context.Context, nodeName string, pod *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, bool)

	// NodeMaybeOverReserved declares a node was filtered out for not enough resources available.
	// This means this node is eligible for a resync. When a node is marked discarded (dirty), it matters not
	// if it is so because pessimistic overallocation or because the node truly cannot accomodate the request;
	// this is for the resync step to figure out.
	// The pod argument is used only for logging purposes.
	NodeMaybeOverReserved(nodeName string, pod *corev1.Pod)

	// NodeHasForeignPods declares we observed one or more pods on this node which wasn't scheduled by this
	// scheduler instance. This means the resource accounting is likely out of date, so this function also signals
	// a cache resync is needed for this node.
	// Until that happens, this node should not be considered in the scheduling decisions, like it has zero resources
	// available. Note this condition is different from "no topology info available".
	// The former is a always-fail, the latter is a always-succeed.
	NodeHasForeignPods(nodeName string, pod *corev1.Pod)

	// ReserveNodeResources add the resources requested by a pod to the assumed resources for the node on which the pod
	// is scheduled on. This is a prerequesite for the pessimistic overallocation tracking.
	// Additionally, this function resets the discarded counter for the same node. Being able to handle a pod means
	// that this node has still available resources. If a node was previously discarded and then cleared, we interpret
	// this sequence of events as the previous pod required too much - a possible and benign condition.
	ReserveNodeResources(nodeName string, pod *corev1.Pod)

	// UnreserveNodeResources decrement from the node assumed resources the resources required by the given pod.
	UnreserveNodeResources(nodeName string, pod *corev1.Pod)

	// PostBind is called after a pod is successfully bound. These plugins are
	// informational. A common application of this extension point is for cleaning
	// up. If a plugin needs to clean-up its state after a pod is scheduled and
	// bound, PostBind is the extension point that it should register.
	PostBind(nodeName string, pod *corev1.Pod)
}
