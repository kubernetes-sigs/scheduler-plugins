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

package nodeconfig

import (
	"fmt"
	"strconv"

	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

const (
	DefaultMaxNUMANodes = 8 // legacy setting and default value for TopologyManager. NOTE: kube doesn't expose this constant

	LimitNUMANodes = 1024 // basic sanitization, but we will likely need to bump soon enough
)

const (
	AttributeScope        = "topologyManagerScope"
	AttributePolicy       = "topologyManagerPolicy"
	AttributeMaxNUMANodes = "topologyManagerMaxNUMANodes"
)

func IsValidScope(scope string) bool {
	if scope == kubeletconfig.ContainerTopologyManagerScope || scope == kubeletconfig.PodTopologyManagerScope {
		return true
	}
	return false
}

func IsValidPolicy(policy string) bool {
	if policy == kubeletconfig.NoneTopologyManagerPolicy || policy == kubeletconfig.BestEffortTopologyManagerPolicy ||
		policy == kubeletconfig.RestrictedTopologyManagerPolicy || policy == kubeletconfig.SingleNumaNodeTopologyManagerPolicy {
		return true
	}
	return false
}

func IsValidMaxNUMANodes(value int) bool {
	// machines always report at least 1 NUMA node anyway, and furthermore the value is used in a division,
	// so we need to enforce 1 (not 0) as minimum
	// NOTE: there's no legit upper bound, so care must be taken to cap this value. But still, theoretically
	// 4096 NUMA nodes is a valid legal value.
	return value > 1
}

type TopologyManager struct {
	Scope        string
	Policy       string
	MaxNUMANodes int
}

func TopologyManagerDefaults() TopologyManager {
	return TopologyManager{
		Scope:        kubeletconfig.ContainerTopologyManagerScope,
		Policy:       kubeletconfig.NoneTopologyManagerPolicy,
		MaxNUMANodes: DefaultMaxNUMANodes,
	}
}

func TopologyManagerFromNodeResourceTopology(lh logr.Logger, nodeTopology *topologyv1alpha2.NodeResourceTopology) TopologyManager {
	conf := TopologyManagerDefaults()
	cfg := &conf // shortcut
	// Backward compatibility (v1alpha2 and previous). Deprecated, will be removed when the NRT API moves to v1beta1.
	cfg.updateFromPolicies(lh, nodeTopology.Name, nodeTopology.TopologyPolicies)
	// preferred new configuration source (v1alpha2 and onwards)
	cfg.updateFromAttributes(lh, nodeTopology.Attributes)
	return conf
}

func (conf TopologyManager) String() string {
	return fmt.Sprintf("policy=%s scope=%s maxNUMANodes=%d", conf.Policy, conf.Scope, conf.MaxNUMANodes)
}

func (conf TopologyManager) Equal(other TopologyManager) bool {
	if conf.Scope != other.Scope {
		return false
	}
	if conf.Policy != other.Policy {
		return false
	}
	return conf.MaxNUMANodes == other.MaxNUMANodes
}

func (conf *TopologyManager) updateFromAttributes(lh logr.Logger, attrs topologyv1alpha2.AttributeList) {
	for _, attr := range attrs {
		if attr.Name == AttributeScope && IsValidScope(attr.Value) {
			conf.Scope = attr.Value
			continue
		}
		if attr.Name == AttributePolicy && IsValidPolicy(attr.Value) {
			conf.Policy = attr.Value
			continue
		}
		if attr.Name == AttributeMaxNUMANodes {
			if val, err := strconv.Atoi(attr.Value); err == nil && IsValidMaxNUMANodes(val) {
				conf.MaxNUMANodes = clampMaxNUMANodes(lh, val)
				continue
			}
		}
	}
}

func clampMaxNUMANodes(lh logr.Logger, val int) int {
	if val > LimitNUMANodes {
		// should never happen, so we are verbose
		lh.Info("capped MaxNUMANodes value to limit", "value", val, "limit", LimitNUMANodes)
		val = LimitNUMANodes
	}
	return val
}

func (conf *TopologyManager) updateFromPolicies(lh logr.Logger, nodeName string, topologyPolicies []string) {
	if len(topologyPolicies) == 0 {
		return
	}
	if len(topologyPolicies) > 1 {
		lh.V(4).Info("ignoring extra policies", "node", nodeName, "policies count", len(topologyPolicies)-1)
	}

	policyName := topologyv1alpha2.TopologyManagerPolicy(topologyPolicies[0])
	lh.Info("the `topologyPolicies` field is deprecated and will be removed with the NRT API v1beta1.")
	lh.Info("the `topologyPolicies` field is deprecated, please use top-level Attributes field instead.")

	switch policyName {
	case topologyv1alpha2.SingleNUMANodePodLevel:
		conf.Policy = kubeletconfig.SingleNumaNodeTopologyManagerPolicy
		conf.Scope = kubeletconfig.PodTopologyManagerScope
	case topologyv1alpha2.SingleNUMANodeContainerLevel:
		conf.Policy = kubeletconfig.SingleNumaNodeTopologyManagerPolicy
		conf.Scope = kubeletconfig.ContainerTopologyManagerScope
	case topologyv1alpha2.BestEffortPodLevel:
		conf.Policy = kubeletconfig.BestEffortTopologyManagerPolicy
		conf.Scope = kubeletconfig.PodTopologyManagerScope
	case topologyv1alpha2.BestEffortContainerLevel:
		conf.Policy = kubeletconfig.BestEffortTopologyManagerPolicy
		conf.Scope = kubeletconfig.ContainerTopologyManagerScope
	case topologyv1alpha2.RestrictedPodLevel:
		conf.Policy = kubeletconfig.RestrictedTopologyManagerPolicy
		conf.Scope = kubeletconfig.PodTopologyManagerScope
	case topologyv1alpha2.RestrictedContainerLevel:
		conf.Policy = kubeletconfig.RestrictedTopologyManagerPolicy
		conf.Scope = kubeletconfig.ContainerTopologyManagerScope
	}
}
