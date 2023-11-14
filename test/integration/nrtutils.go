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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

const (
	cpu             = string(corev1.ResourceCPU)
	memory          = string(corev1.ResourceMemory)
	gpuResourceName = "vendor/gpu"
	hugepages2Mi    = "hugepages-2Mi"
	nicResourceName = "vendor/nic1"
)

func waitForNRT(cs *clientset.Clientset) error {
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

func createNodesFromNodeResourceTopologies(cs clientset.Interface, ctx context.Context, nodeResourceTopologies []*topologyv1alpha2.NodeResourceTopology) error {
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

func createNodeResourceTopologies(ctx context.Context, client ctrlclient.Client, noderesourcetopologies []*topologyv1alpha2.NodeResourceTopology) error {
	for _, nrt := range noderesourcetopologies {
		if err := client.Create(ctx, nrt.DeepCopy()); err != nil {
			return err
		}
	}
	return nil
}

func updateNodeResourceTopologies(ctx context.Context, client ctrlclient.Client, noderesourcetopologies []*topologyv1alpha2.NodeResourceTopology) error {
	for _, nrt := range noderesourcetopologies {
		updatedNrt := &topologyv1alpha2.NodeResourceTopology{}
		if err := client.Get(ctx, types.NamespacedName{Name: nrt.Name}, updatedNrt); err != nil {
			return err
		}

		obj := updatedNrt.DeepCopy()
		if obj.Annotations == nil {
			obj.Annotations = make(map[string]string)
		}
		for key, value := range nrt.Annotations {
			obj.Annotations[key] = value
		}

		obj.TopologyPolicies = nrt.TopologyPolicies // TODO: Deprecated; shallow copy
		obj.Attributes = nrt.Attributes.DeepCopy()
		obj.Zones = nrt.Zones.DeepCopy()

		if err := client.Update(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

func cleanupNodeResourceTopologies(ctx context.Context, client ctrlclient.Client, noderesourcetopologies []*topologyv1alpha2.NodeResourceTopology) {
	for _, nrt := range noderesourcetopologies {
		if err := client.Delete(ctx, nrt); err != nil {
			klog.ErrorS(err, "Failed to clean up NodeResourceTopology",
				"nodeResourceTopology", nrt)
		}
	}
	klog.Infof("cleaned up NRT %d objects", len(noderesourcetopologies))
}

func makeResourceAllocationScoreArgs(strategy *scheconfig.ScoringStrategy) *scheconfig.NodeResourceTopologyMatchArgs {
	return &scheconfig.NodeResourceTopologyMatchArgs{
		ScoringStrategy: *strategy,
	}
}

type nrtWrapper struct {
	nrt topologyv1alpha2.NodeResourceTopology
}

func MakeNRT() *nrtWrapper {
	return &nrtWrapper{topologyv1alpha2.NodeResourceTopology{}}
}

func (n *nrtWrapper) Name(name string) *nrtWrapper {
	n.nrt.Name = name
	return n
}

func (n *nrtWrapper) Policy(policy topologyv1alpha2.TopologyManagerPolicy) *nrtWrapper {
	n.nrt.TopologyPolicies = append(n.nrt.TopologyPolicies, string(policy))
	return n
}

func (n *nrtWrapper) Zone(resInfo topologyv1alpha2.ResourceInfoList) *nrtWrapper {
	z := topologyv1alpha2.Zone{
		Name:      fmt.Sprintf("node-%d", len(n.nrt.Zones)),
		Type:      "Node",
		Resources: resInfo,
	}
	n.nrt.Zones = append(n.nrt.Zones, z)
	return n
}

func (n *nrtWrapper) ZoneWithCosts(resInfo topologyv1alpha2.ResourceInfoList, costs topologyv1alpha2.CostList) *nrtWrapper {
	z := topologyv1alpha2.Zone{
		Name:      fmt.Sprintf("node-%d", len(n.nrt.Zones)),
		Type:      "Node",
		Resources: resInfo,
		Costs:     costs,
	}
	n.nrt.Zones = append(n.nrt.Zones, z)
	return n
}

func (n *nrtWrapper) Annotations(anns map[string]string) *nrtWrapper {
	if n.nrt.Annotations == nil {
		n.nrt.Annotations = make(map[string]string)
	}
	for key, val := range anns {
		n.nrt.Annotations[key] = val
	}
	return n
}

func (n *nrtWrapper) Attributes(attrs topologyv1alpha2.AttributeList) *nrtWrapper {
	n.nrt.Attributes = append(n.nrt.Attributes, attrs...)
	return n
}

func (n *nrtWrapper) Obj() *topologyv1alpha2.NodeResourceTopology {
	return &n.nrt
}

func podIsScheduled(interval time.Duration, times int, cs clientset.Interface, podNamespace, podName string) (*corev1.Pod, error) {
	var err error
	var pod *corev1.Pod
	waitErr := wait.Poll(interval, time.Duration(times)*interval, func() (bool, error) {
		pod, err = cs.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			// This could be a connection error so we want to retry.
			klog.ErrorS(err, "Failed to get pod", "pod", klog.KRef(podNamespace, podName))
			return false, err
		}
		return pod.Spec.NodeName != "", nil
	})
	return pod, waitErr
}

func podIsPending(interval time.Duration, times int, cs clientset.Interface, podNamespace, podName string) (*corev1.Pod, error) {
	var err error
	var pod *corev1.Pod
	for attempt := 0; attempt < times; attempt++ {
		pod, err = cs.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return pod, fmt.Errorf("pod %s/%s not pending: %w", podNamespace, podName, err)
		}
		if pod.Spec.NodeName != "" {
			return pod, fmt.Errorf("pod %s/%s not pending: bound to %q", podNamespace, podName, pod.Spec.NodeName)
		}
		time.Sleep(interval)
	}
	return pod, nil
}

func accumulateResourcesToCapacity(nrt topologyv1alpha2.NodeResourceTopology) corev1.ResourceList {
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

func makeTestFullyAvailableNRTSingle() []*topologyv1alpha2.NodeResourceTopology {
	return []*topologyv1alpha2.NodeResourceTopology{
		MakeNRT().Name("fake-node-cache-1").Policy(topologyv1alpha2.SingleNUMANodeContainerLevel).
			Zone(
				topologyv1alpha2.ResourceInfoList{
					noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
					noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "62Gi"),
				}).
			Zone(
				topologyv1alpha2.ResourceInfoList{
					noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
					noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "62Gi"),
				}).Obj(),
	}
}

func makeTestFullyAvailableNRTs() []*topologyv1alpha2.NodeResourceTopology {
	nrts := makeTestFullyAvailableNRTSingle()
	return append(nrts,
		MakeNRT().Name("fake-node-cache-2").Policy(topologyv1alpha2.SingleNUMANodeContainerLevel).
			Zone(
				topologyv1alpha2.ResourceInfoList{
					noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
					noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "62Gi"),
					noderesourcetopology.MakeTopologyResInfo(nicResourceName, "2", "2"),
				}).
			Zone(
				topologyv1alpha2.ResourceInfoList{
					noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
					noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "62Gi"),
					noderesourcetopology.MakeTopologyResInfo(nicResourceName, "2", "2"),
				}).Obj(),
	)
}

func formatObject(obj interface{}) string {
	bytes, err := json.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("<ERROR: %s>", err)
	}
	return string(bytes)
}
