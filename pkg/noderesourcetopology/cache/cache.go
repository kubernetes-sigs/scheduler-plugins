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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	listerv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha1"
)

type Cache interface {
	// GetByNode returns the last Node Resource Topology info available for a node, adjusted with the assumed
	// resources for that node. Assumed resources are the resources consumed by pods scheduled to that node
	// after the last update of NRT pertaining to the same node, pessimistically overallocated on ALL the NUMA
	// zones of the node.
	// The pod argument is used only for logging purposes.
	GetByNode(nodeName string, pod *corev1.Pod) *topologyv1alpha1.NodeResourceTopology

	// MarkNodeDiscarded declares a node was filtered out for not enough resources available.
	// This means this node is eligible for a resync. When a node is marked discarded (dirty), it matters not
	// if it is so because pessimistic overallocation or because the node truly cannot accomodate the request;
	// this is for the resync step to figure out.
	// The pod argument is used only for logging purposes.
	MarkNodeDiscarded(nodeName string, pod *corev1.Pod)

	// ReserveNodeResources add the resources requested by a pod to the assumed resources for the node on which the pod
	// is scheduled on. This is a prerequesite for the pessimistic overallocation tracking.
	// Additionally, this function resets the discarded counter for the same node. Being able to handle a pod means
	// that this node has still available resources. If a node was previously discarded and then cleared, we interpret
	// this sequence of events as the previous pod required too much - a possible and benign condition.
	ReserveNodeResources(nodeName string, pod *corev1.Pod)

	// UnreserveNodeResources decrement from the node assumed resources the resources required by the given pod.
	UnreserveNodeResources(nodeName string, pod *corev1.Pod)
}

type Passthrough struct {
	lister listerv1alpha1.NodeResourceTopologyLister
}

func NewPassthrough(lister listerv1alpha1.NodeResourceTopologyLister) Passthrough {
	return Passthrough{
		lister: lister,
	}
}

func (pt Passthrough) GetByNode(nodeName string, _ *corev1.Pod) *topologyv1alpha1.NodeResourceTopology {
	klog.V(5).InfoS("Lister for nodeResTopoPlugin", "lister", pt.lister)
	nrt, err := pt.lister.Get(nodeName)
	if err != nil {
		klog.V(5).ErrorS(err, "Cannot get NodeTopologies from NodeResourceTopologyLister")
		return nil
	}
	return nrt
}

func (pt Passthrough) MarkNodeDiscarded(nodeName string, pod *corev1.Pod)      {}
func (pt Passthrough) ReserveNodeResources(nodeName string, pod *corev1.Pod)   {}
func (pt Passthrough) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {}
