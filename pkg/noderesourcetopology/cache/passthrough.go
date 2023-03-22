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

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	listerv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha2"
)

type Passthrough struct {
	lister listerv1alpha2.NodeResourceTopologyLister
}

func NewPassthrough(lister listerv1alpha2.NodeResourceTopologyLister) Interface {
	return Passthrough{
		lister: lister,
	}
}

func (pt Passthrough) GetCachedNRTCopy(nodeName string, _ *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, bool) {
	klog.V(5).InfoS("Lister for nodeResTopoPlugin", "lister", pt.lister)
	nrt, err := pt.lister.Get(nodeName)
	if err != nil {
		klog.V(5).ErrorS(err, "Cannot get NodeTopologies from NodeResourceTopologyLister")
		return nil, true
	}
	return nrt, true
}

func (pt Passthrough) NodeMaybeOverReserved(nodeName string, pod *corev1.Pod)  {}
func (pt Passthrough) NodeHasForeignPods(nodeName string, pod *corev1.Pod)     {}
func (pt Passthrough) ReserveNodeResources(nodeName string, pod *corev1.Pod)   {}
func (pt Passthrough) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {}
func (pt Passthrough) PostBind(nodeName string, pod *corev1.Pod)               {}
