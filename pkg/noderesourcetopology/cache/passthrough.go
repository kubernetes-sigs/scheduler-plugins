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

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Passthrough struct {
	client ctrlclient.Client
	lh     logr.Logger
}

func NewPassthrough(lh logr.Logger, client ctrlclient.Client) Interface {
	return Passthrough{
		client: client,
		lh:     lh,
	}
}

func (pt Passthrough) GetCachedNRTCopy(ctx context.Context, nodeName string, _ *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, CachedNRTInfo) {
	pt.lh.V(5).Info("lister for NRT plugin")
	info := CachedNRTInfo{Fresh: true}
	nrt := &topologyv1alpha2.NodeResourceTopology{}
	if err := pt.client.Get(ctx, types.NamespacedName{Name: nodeName}, nrt); err != nil {
		pt.lh.V(5).Error(err, "cannot get nrts from lister")
		return nil, info
	}
	return nrt, info
}

func (pt Passthrough) NodeMaybeOverReserved(nodeName string, pod *corev1.Pod)  {}
func (pt Passthrough) NodeHasForeignPods(nodeName string, pod *corev1.Pod)     {}
func (pt Passthrough) ReserveNodeResources(nodeName string, pod *corev1.Pod)   {}
func (pt Passthrough) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {}
func (pt Passthrough) PostBind(nodeName string, pod *corev1.Pod)               {}
