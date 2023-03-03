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

package noderesourcetopology

import (
	"k8s.io/klog/v2"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

const (
	AttributeScope  = "topologyManagerScope"
	AttributePolicy = "topologyManagerPolicy"
)

// TODO: handle topologyManagerPolicyOptions added in k8s 1.26

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

type TopologyManagerConfig struct {
	Scope  string
	Policy string
}

func makeTopologyManagerConfigDefaults() TopologyManagerConfig {
	return TopologyManagerConfig{
		Scope:  kubeletconfig.ContainerTopologyManagerScope,
		Policy: kubeletconfig.NoneTopologyManagerPolicy,
	}
}

func topologyManagerConfigFromNodeResourceTopology(nodeTopology *topologyv1alpha2.NodeResourceTopology) TopologyManagerConfig {
	conf := makeTopologyManagerConfigDefaults()
	// Backward compatibility (v1alpha2 and previous). Deprecated, will be removed when the NRT API moves to v1beta1.
	updateTopologyManagerConfigFromTopologyPolicies(&conf, nodeTopology.Name, nodeTopology.TopologyPolicies)
	// preferred new configuration source (v1alpha2 and onwards)
	updateTopologyManagerConfigFromAttributes(&conf, nodeTopology.Attributes)
	return conf
}

func updateTopologyManagerConfigFromAttributes(conf *TopologyManagerConfig, attrs topologyv1alpha2.AttributeList) {
	for _, attr := range attrs {
		if attr.Name == AttributeScope && IsValidScope(attr.Value) {
			conf.Scope = attr.Value
			continue
		}
		if attr.Name == AttributePolicy && IsValidPolicy(attr.Value) {
			conf.Policy = attr.Value
			continue
		}
		// TODO: handle topologyManagerPolicyOptions added in k8s 1.26
	}
}

func updateTopologyManagerConfigFromTopologyPolicies(conf *TopologyManagerConfig, nodeName string, topologyPolicies []string) {
	if len(topologyPolicies) == 0 {
		klog.V(3).InfoS("Cannot determine policy", "node", nodeName)
		return
	}
	if len(topologyPolicies) > 1 {
		klog.V(4).InfoS("Ignoring extra policies", "node", nodeName, "policies count", len(topologyPolicies)-1)
	}

	policyName := topologyv1alpha2.TopologyManagerPolicy(topologyPolicies[0])
	klog.Warning("The `topologyPolicies` field is deprecated and will be removed with the NRT API v1beta1.")
	klog.Warning("The `topologyPolicies` field is deprecated, please use top-level Attributes field instead.")

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
