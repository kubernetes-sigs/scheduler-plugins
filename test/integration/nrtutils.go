/*
Copyright 2021 The Kubernetes Authors.

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

package integration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
)

const (
	cpu             = string(corev1.ResourceCPU)
	memory          = string(corev1.ResourceMemory)
	hugepages2Mi    = "hugepages-2Mi"
	nicResourceName = "vendor/nic1"
)

// getNodeName returns the name of the node if a node has assigned to the given pod
func getNodeName(ctx context.Context, c clientset.Interface, podNamespace, podName string) (string, error) {
	pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Spec.NodeName, nil
}

func createNodeResourceTopologies(ctx context.Context, topologyClient *versioned.Clientset, noderesourcetopologies []*topologyv1alpha1.NodeResourceTopology) error {
	for _, nrt := range noderesourcetopologies {
		_, err := topologyClient.TopologyV1alpha1().NodeResourceTopologies().Create(ctx, nrt, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupNodeResourceTopologies(ctx context.Context, topologyClient *versioned.Clientset, noderesourcetopologies []*topologyv1alpha1.NodeResourceTopology) {
	for _, nrt := range noderesourcetopologies {
		err := topologyClient.TopologyV1alpha1().NodeResourceTopologies().Delete(ctx, nrt.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to clean up NodeResourceTopology", "nodeResourceTopology", nrt)
		}
	}
	klog.Infof("init scheduler success")
}

func makeResourceAllocationScoreArgs(strategy *scheconfig.ScoringStrategy) *scheconfig.NodeResourceTopologyMatchArgs {
	return &scheconfig.NodeResourceTopologyMatchArgs{
		ScoringStrategy: *strategy,
	}
}

type nrtWrapper struct {
	nrt topologyv1alpha1.NodeResourceTopology
}

func MakeNRT() *nrtWrapper {
	return &nrtWrapper{topologyv1alpha1.NodeResourceTopology{}}
}

func (n *nrtWrapper) Name(name string) *nrtWrapper {
	n.nrt.Name = name
	return n
}

func (n *nrtWrapper) Policy(policy topologyv1alpha1.TopologyManagerPolicy) *nrtWrapper {
	n.nrt.TopologyPolicies = append(n.nrt.TopologyPolicies, string(policy))
	return n
}

func (n *nrtWrapper) Zone(resInfo topologyv1alpha1.ResourceInfoList) *nrtWrapper {
	z := topologyv1alpha1.Zone{
		Name:      fmt.Sprintf("node-%d", len(n.nrt.Zones)),
		Type:      "Node",
		Resources: resInfo,
	}
	n.nrt.Zones = append(n.nrt.Zones, z)
	return n
}

func (n *nrtWrapper) Obj() *topologyv1alpha1.NodeResourceTopology {
	return &n.nrt
}
