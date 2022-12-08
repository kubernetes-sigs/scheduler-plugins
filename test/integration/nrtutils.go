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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
)

const (
	cpu             = string(corev1.ResourceCPU)
	memory          = string(corev1.ResourceMemory)
	hugepages2Mi    = "hugepages-2Mi"
	nicResourceName = "vendor/nic1"
)

func waitForNRT(cs *kubernetes.Clientset) error {
	return wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == "topology.node.k8s.io" {
				klog.Infof("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	})
}

func createNodesFromNodeResourceTopologies(cs clientset.Interface, ctx context.Context, nodeResourceTopologies []*topologyv1alpha1.NodeResourceTopology) error {
	for _, nrt := range nodeResourceTopologies {
		nodeName := nrt.Name
		resMap := toResMap(accumulateResourcesToCapacity(*nrt))
		resMap[corev1.ResourcePods] = "128"

		klog.Infof(" Creating node %q: %s", nodeName, resMapToString(resMap))

		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).Capacity(resMap).Obj()
		n, err := cs.CoreV1().Nodes().Create(ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("Failed to create Node %q: %w", nodeName, err)
		}

		klog.Infof(" Node %s created: (%s | %s)", nodeName, stringify.ResourceList(n.Status.Capacity), stringify.ResourceList(n.Status.Allocatable))
	}
	return nil
}

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

func podIsScheduled(interval time.Duration, times int, cs clientset.Interface, podNamespace, podName string) error {
	return wait.Poll(interval, time.Duration(times)*interval, func() (bool, error) {
		return podScheduled(cs, podNamespace, podName), nil
	})
}

func podIsPending(interval time.Duration, times int, cs clientset.Interface, podNamespace, podName string) error {
	for attempt := 0; attempt < times; attempt++ {
		if !podPending(cs, podNamespace, podName) {
			return fmt.Errorf("pod %s/%s not pending", podNamespace, podName)
		}
		time.Sleep(interval)
	}
	return nil
}

// podPending returns true if a node is not assigned to the given pod.
func podPending(c clientset.Interface, podNamespace, podName string) bool {
	pod, err := c.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		// This could be a connection error so we want to retry.
		klog.ErrorS(err, "Failed to get pod", "pod", klog.KRef(podNamespace, podName))
		return false
	}
	return pod.Spec.NodeName == ""
}

func accumulateResourcesToCapacity(nrt topologyv1alpha1.NodeResourceTopology) corev1.ResourceList {
	resList := corev1.ResourceList{}
	for _, zone := range nrt.Zones {
		for _, res := range zone.Resources {
			if q, ok := resList[corev1.ResourceName(res.Name)]; ok {
				q.Add(res.Capacity)
				resList[corev1.ResourceName(res.Name)] = q
			} else {
				resList[corev1.ResourceName(res.Name)] = res.Capacity
			}
		}
	}
	return resList
}

func toResMap(resList corev1.ResourceList) map[corev1.ResourceName]string {
	ret := map[corev1.ResourceName]string{}
	for name, qty := range resList {
		ret[name] = qty.String()
	}
	return ret
}

func resMapToString(resMap map[corev1.ResourceName]string) string {
	resNames := []string{}
	for resName := range resMap {
		resNames = append(resNames, string(resName))
	}
	sort.Strings(resNames)

	resItems := []string{}
	for _, resName := range resNames {
		qty := resMap[corev1.ResourceName(resName)]
		resItems = append(resItems, resName+"="+qty)
	}
	return strings.Join(resItems, ",")
}
