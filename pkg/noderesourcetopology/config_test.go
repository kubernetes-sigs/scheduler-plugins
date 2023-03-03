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
	"reflect"
	"testing"

	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

func TestIsValidScope(t *testing.T) {
	tests := []struct {
		scope    string
		expected bool
	}{
		{
			scope:    "",
			expected: false,
		},
		{
			scope:    kubeletconfig.PodTopologyManagerScope,
			expected: true,
		},
		{
			scope:    kubeletconfig.ContainerTopologyManagerScope,
			expected: true,
		},
		{
			scope:    "POD",
			expected: false,
		},
		{
			scope:    "Container",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.scope, func(t *testing.T) {
			got := IsValidScope(tt.scope)
			if got != tt.expected {
				t.Errorf("scope=%s got=%v expected=%v", tt.scope, got, tt.expected)
			}
		})
	}
}

func TestIsValidPolicy(t *testing.T) {
	tests := []struct {
		policy   string
		expected bool
	}{
		{
			policy:   "",
			expected: false,
		},
		{
			policy:   kubeletconfig.NoneTopologyManagerPolicy,
			expected: true,
		},
		{
			policy:   kubeletconfig.BestEffortTopologyManagerPolicy,
			expected: true,
		},
		{
			policy:   kubeletconfig.RestrictedTopologyManagerPolicy,
			expected: true,
		},
		{
			policy:   kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			expected: true,
		},
		{
			policy:   "None",
			expected: false,
		},
		{
			policy:   "BestEffort",
			expected: false,
		},
		{
			policy:   "Restricted",
			expected: false,
		},
		{
			policy:   "single-NUMA-node",
			expected: false,
		},
		{
			policy:   "SingleNUMANode",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			got := IsValidPolicy(tt.policy)
			if got != tt.expected {
				t.Errorf("policy=%s got=%v expected=%v", tt.policy, got, tt.expected)
			}
		})
	}
}

func TestConfigFromAttributes(t *testing.T) {
	tests := []struct {
		name     string
		attrs    topologyv1alpha2.AttributeList
		expected TopologyManagerConfig
	}{
		{
			name:     "nil",
			attrs:    nil,
			expected: TopologyManagerConfig{},
		},
		{
			name:     "empty",
			attrs:    topologyv1alpha2.AttributeList{},
			expected: TopologyManagerConfig{},
		},
		{
			name: "no-policy",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerScope",
					Value: "pod",
				},
			},
			expected: TopologyManagerConfig{
				Scope: kubeletconfig.PodTopologyManagerScope,
			},
		},
		{
			name: "no-scope",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerPolicy",
					Value: "restricted",
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
			},
		},
		{
			name: "complete-case-1",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerPolicy",
					Value: "restricted",
				},
				{
					Name:  "topologyManagerScope",
					Value: "container",
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name: "complete-case-2",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerScope",
					Value: "pod",
				},
				{
					Name:  "topologyManagerPolicy",
					Value: "single-numa-node",
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
				Scope:  kubeletconfig.PodTopologyManagerScope,
			},
		},
		{
			name: "error-case-1",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerScope",
					Value: "Pod",
				},
				{
					Name:  "topologyManagerPolicy",
					Value: "single-numa-node",
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
			},
		},
		{
			name: "error-case-2",
			attrs: topologyv1alpha2.AttributeList{
				{
					Name:  "topologyManagerScope",
					Value: "Container",
				},
				{
					Name:  "topologyManagerPolicy",
					Value: "restricted",
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TopologyManagerConfig{}
			updateTopologyManagerConfigFromAttributes(&got, tt.attrs)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("conf got=%+#v expected=%+#v", got, tt.expected)
			}
		})
	}
}

func TestConfigFromPolicies(t *testing.T) {
	tests := []struct {
		name     string
		policies []string
		expected TopologyManagerConfig
	}{
		{
			name:     "nil",
			policies: nil,
			expected: TopologyManagerConfig{},
		},
		{
			name:     "empty",
			policies: []string{},
			expected: TopologyManagerConfig{},
		},
		{
			name:     "single-numa-pod",
			policies: []string{string(topologyv1alpha2.SingleNUMANodePodLevel)},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
				Scope:  kubeletconfig.PodTopologyManagerScope,
			},
		},
		{
			name:     "single-numa-container",
			policies: []string{string(topologyv1alpha2.SingleNUMANodeContainerLevel)},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name:     "restricted-container",
			policies: []string{string(topologyv1alpha2.RestrictedContainerLevel)},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name: "skip-policies",
			policies: []string{
				string(topologyv1alpha2.RestrictedContainerLevel),
				string(topologyv1alpha2.SingleNUMANodePodLevel),
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name:     "error-unknown-policy",
			policies: []string{"foobar"},
			expected: TopologyManagerConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TopologyManagerConfig{}
			updateTopologyManagerConfigFromTopologyPolicies(&got, "", tt.policies)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("conf got=%+#v expected=%+#v", got, tt.expected)
			}
		})
	}
}

func TestConfigFromNRT(t *testing.T) {
	tests := []struct {
		name     string
		nrt      topologyv1alpha2.NodeResourceTopology
		expected TopologyManagerConfig
	}{
		{
			name:     "nil",
			nrt:      topologyv1alpha2.NodeResourceTopology{},
			expected: makeTopologyManagerConfigDefaults(),
		},
		{
			name: "policies-single",
			nrt: topologyv1alpha2.NodeResourceTopology{
				TopologyPolicies: []string{
					string(topologyv1alpha2.BestEffortPodLevel),
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.BestEffortTopologyManagerPolicy,
				Scope:  kubeletconfig.PodTopologyManagerScope,
			},
		},
		{
			name: "policies-ignore-after-first",
			nrt: topologyv1alpha2.NodeResourceTopology{
				TopologyPolicies: []string{
					string(topologyv1alpha2.RestrictedContainerLevel),
					string(topologyv1alpha2.BestEffortPodLevel),
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name: "attributes-partial-policy-only",
			nrt: topologyv1alpha2.NodeResourceTopology{
				Attributes: topologyv1alpha2.AttributeList{
					{
						Name:  "topologyManagerPolicy",
						Value: "restricted",
					},
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name: "attributes-overrides-policy-partial",
			nrt: topologyv1alpha2.NodeResourceTopology{
				TopologyPolicies: []string{
					string(topologyv1alpha2.BestEffortPodLevel),
				},
				Attributes: topologyv1alpha2.AttributeList{
					{
						Name:  "topologyManagerScope",
						Value: "container",
					},
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.BestEffortTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
		{
			name: "attributes-overrides-policy-full",
			nrt: topologyv1alpha2.NodeResourceTopology{
				TopologyPolicies: []string{
					string(topologyv1alpha2.BestEffortPodLevel),
				},
				Attributes: topologyv1alpha2.AttributeList{
					{
						Name:  "topologyManagerScope",
						Value: "container",
					},
					{
						Name:  "topologyManagerPolicy",
						Value: "restricted",
					},
				},
			},
			expected: TopologyManagerConfig{
				Policy: kubeletconfig.RestrictedTopologyManagerPolicy,
				Scope:  kubeletconfig.ContainerTopologyManagerScope,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topologyManagerConfigFromNodeResourceTopology(&tt.nrt)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("conf got=%+#v expected=%+#v", got, tt.expected)
			}
		})
	}

}
