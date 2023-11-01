/*
Copyright 2020 The Kubernetes Authors.

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

package util

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

var _ framework.SharedLister = &fakeSharedLister{}

type fakeSharedLister struct {
	nodeInfos                                    []*framework.NodeInfo
	nodeInfoMap                                  map[string]*framework.NodeInfo
	havePodsWithAffinityNodeInfoList             []*framework.NodeInfo
	havePodsWithRequiredAntiAffinityNodeInfoList []*framework.NodeInfo
}

func NewFakeSharedLister(pods []*v1.Pod, nodes []*v1.Node) framework.SharedLister {
	nodeInfoMap := createNodeInfoMap(pods, nodes)
	nodeInfos := make([]*framework.NodeInfo, 0, len(nodeInfoMap))
	havePodsWithAffinityNodeInfoList := make([]*framework.NodeInfo, 0, len(nodeInfoMap))
	havePodsWithRequiredAntiAffinityNodeInfoList := make([]*framework.NodeInfo, 0, len(nodeInfoMap))
	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
		if len(v.PodsWithAffinity) > 0 {
			havePodsWithAffinityNodeInfoList = append(havePodsWithAffinityNodeInfoList, v)
		}
		if len(v.PodsWithRequiredAntiAffinity) > 0 {
			havePodsWithRequiredAntiAffinityNodeInfoList = append(havePodsWithRequiredAntiAffinityNodeInfoList, v)
		}
	}
	return &fakeSharedLister{
		nodeInfos:                        nodeInfos,
		nodeInfoMap:                      nodeInfoMap,
		havePodsWithAffinityNodeInfoList: havePodsWithAffinityNodeInfoList,
		havePodsWithRequiredAntiAffinityNodeInfoList: havePodsWithRequiredAntiAffinityNodeInfoList,
	}
}

func createNodeInfoMap(pods []*v1.Pod, nodes []*v1.Node) map[string]*framework.NodeInfo {
	nodeNameToInfo := make(map[string]*framework.NodeInfo)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeNameToInfo[nodeName]; !ok {
			nodeNameToInfo[nodeName] = framework.NewNodeInfo()
		}
		nodeNameToInfo[nodeName].AddPod(pod)
	}

	for _, node := range nodes {
		if _, ok := nodeNameToInfo[node.Name]; !ok {
			nodeNameToInfo[node.Name] = framework.NewNodeInfo()
		}
		nodeInfo := nodeNameToInfo[node.Name]
		nodeInfo.SetNode(node)
	}
	return nodeNameToInfo
}

func (f *fakeSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *fakeSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *fakeSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *fakeSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return f.havePodsWithAffinityNodeInfoList, nil
}

func (f *fakeSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) {
	return f.havePodsWithRequiredAntiAffinityNodeInfoList, nil
}

func (f *fakeSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

/*
 * Copied from upstream.
 */

// nominator is a structure that stores pods nominated to run on nodes.
// It exists because nominatedNodeName of pod objects stored in the structure
// may be different than what scheduler has here. We should be able to find pods
// by their UID and update/delete them.
type nominator struct {
	// podLister is used to verify if the given pod is alive.
	podLister listersv1.PodLister
	// nominatedPods is a map keyed by a node name and the value is a list of
	// pods which are nominated to run on the node. These are pods which can be in
	// the activeQ or unschedulableQ.
	nominatedPods map[string][]*framework.PodInfo
	// nominatedPodToNode is map keyed by a Pod UID to the node name where it is
	// nominated.
	nominatedPodToNode map[types.UID]string

	sync.RWMutex
}

func (n *nominator) add(pi *framework.PodInfo, nominatingInfo *framework.NominatingInfo) {
	// Always delete the pod if it already exists, to ensure we never store more than
	// one instance of the pod.
	n.delete(pi.Pod)

	var nodeName string
	if nominatingInfo.Mode() == framework.ModeOverride {
		nodeName = nominatingInfo.NominatedNodeName
	} else if nominatingInfo.Mode() == framework.ModeNoop {
		if pi.Pod.Status.NominatedNodeName == "" {
			return
		}
		nodeName = pi.Pod.Status.NominatedNodeName
	}

	if n.podLister != nil {
		// If the pod is not alive, don't contain it.
		if _, err := n.podLister.Pods(pi.Pod.Namespace).Get(pi.Pod.Name); err != nil {
			klog.V(4).InfoS("Pod doesn't exist in podLister, aborting adding it to the nominator", "pod", klog.KObj(pi.Pod))
			return
		}
	}

	n.nominatedPodToNode[pi.Pod.UID] = nodeName
	for _, npi := range n.nominatedPods[nodeName] {
		if npi.Pod.UID == pi.Pod.UID {
			klog.V(4).InfoS("Pod already exists in the nominator", "pod", klog.KObj(npi.Pod))
			return
		}
	}
	n.nominatedPods[nodeName] = append(n.nominatedPods[nodeName], pi)
}

func (n *nominator) delete(p *v1.Pod) {
	nnn, ok := n.nominatedPodToNode[p.UID]
	if !ok {
		return
	}
	for i, np := range n.nominatedPods[nnn] {
		if np.Pod.UID == p.UID {
			n.nominatedPods[nnn] = append(n.nominatedPods[nnn][:i], n.nominatedPods[nnn][i+1:]...)
			if len(n.nominatedPods[nnn]) == 0 {
				delete(n.nominatedPods, nnn)
			}
			break
		}
	}
	delete(n.nominatedPodToNode, p.UID)
}

// UpdateNominatedPod updates the <oldPod> with <newPod>.
func (n *nominator) UpdateNominatedPod(oldPod *v1.Pod, newPodInfo *framework.PodInfo) {
	n.Lock()
	defer n.Unlock()
	// In some cases, an Update event with no "NominatedNode" present is received right
	// after a node("NominatedNode") is reserved for this pod in memory.
	// In this case, we need to keep reserving the NominatedNode when updating the pod pointer.
	var nominatingInfo *framework.NominatingInfo
	// We won't fall into below `if` block if the Update event represents:
	// (1) NominatedNode info is added
	// (2) NominatedNode info is updated
	// (3) NominatedNode info is removed
	if NominatedNodeName(oldPod) == "" && NominatedNodeName(newPodInfo.Pod) == "" {
		if nnn, ok := n.nominatedPodToNode[oldPod.UID]; ok {
			// This is the only case we should continue reserving the NominatedNode
			nominatingInfo = &framework.NominatingInfo{
				NominatingMode:    framework.ModeOverride,
				NominatedNodeName: nnn,
			}
		}
	}
	// We update irrespective of the nominatedNodeName changed or not, to ensure
	// that pod pointer is updated.
	n.delete(oldPod)
	n.add(newPodInfo, nominatingInfo)
}

// DeleteNominatedPodIfExists deletes <pod> from nominatedPods.
func (n *nominator) DeleteNominatedPodIfExists(pod *v1.Pod) {
	n.Lock()
	n.delete(pod)
	n.Unlock()
}

// AddNominatedPod adds a pod to the nominated pods of the given node.
// This is called during the preemption process after a node is nominated to run
// the pod. We update the structure before sending a request to update the pod
// object to avoid races with the following scheduling cycles.
func (n *nominator) AddNominatedPod(pi *framework.PodInfo, nominatingInfo *framework.NominatingInfo) {
	n.Lock()
	n.add(pi, nominatingInfo)
	n.Unlock()
}

// NominatedPodsForNode returns a copy of pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node.
func (n *nominator) NominatedPodsForNode(nodeName string) []*framework.PodInfo {
	n.RLock()
	defer n.RUnlock()
	// Make a copy of the nominated Pods so the caller can mutate safely.
	pods := make([]*framework.PodInfo, len(n.nominatedPods[nodeName]))
	for i := 0; i < len(pods); i++ {
		pods[i] = n.nominatedPods[nodeName][i].DeepCopy()
	}
	return pods
}

// NewPodNominator creates a nominator as a backing of framework.PodNominator.
// A podLister is passed in so as to check if the pod exists
// before adding its nominatedNode info.
func NewPodNominator(podLister listersv1.PodLister) framework.PodNominator {
	return &nominator{
		podLister:          podLister,
		nominatedPods:      make(map[string][]*framework.PodInfo),
		nominatedPodToNode: make(map[types.UID]string),
	}
}

// NominatedNodeName returns nominated node name of a Pod.
func NominatedNodeName(pod *v1.Pod) string {
	return pod.Status.NominatedNodeName
}

// NewFakeClient returns a generic controller-runtime client with all given `objs` as internal runtime objects.
// It also registers core v1 scheme, this repo's v1alpha1 scheme and topologyv1alpha2 scheme.
// This function is used by unit tests.
func NewFakeClient(objs ...runtime.Object) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := topologyv1alpha2.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(), nil
}

// NewClientOrDie returns a generic controller-runtime client or panic upon any error.
// This function is used by integration tests.
func NewClientOrDie(cfg *rest.Config) client.Client {
	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	return c
}
