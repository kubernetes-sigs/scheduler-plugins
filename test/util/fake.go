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
	"context"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

var _ framework.SharedLister = &fakeSharedLister{}

type fakeSharedLister struct {
	nodeInfos                                    []fwk.NodeInfo
	nodeInfoMap                                  map[string]fwk.NodeInfo
	havePodsWithAffinityNodeInfoList             []fwk.NodeInfo
	havePodsWithRequiredAntiAffinityNodeInfoList []fwk.NodeInfo
}

func NewFakeSharedLister(pods []*v1.Pod, nodes []*v1.Node) framework.SharedLister {
	nodeInfoMap := createNodeInfoMap(pods, nodes)
	nodeInfos := make([]fwk.NodeInfo, 0, len(nodeInfoMap))
	havePodsWithAffinityNodeInfoList := make([]fwk.NodeInfo, 0, len(nodeInfoMap))
	havePodsWithRequiredAntiAffinityNodeInfoList := make([]fwk.NodeInfo, 0, len(nodeInfoMap))
	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
		if len(v.GetPodsWithAffinity()) > 0 {
			havePodsWithAffinityNodeInfoList = append(havePodsWithAffinityNodeInfoList, v)
		}
		if len(v.GetPodsWithRequiredAntiAffinity()) > 0 {
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

func createNodeInfoMap(pods []*v1.Pod, nodes []*v1.Node) map[string]fwk.NodeInfo {
	nodeNameToInfo := make(map[string]fwk.NodeInfo)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeNameToInfo[nodeName]; !ok {
			nodeNameToInfo[nodeName] = framework.NewNodeInfo()
		}
		nodeNameToInfo[nodeName].(*framework.NodeInfo).AddPod(pod)
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

func (f *fakeSharedLister) List() ([]fwk.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *fakeSharedLister) HavePodsWithAffinityList() ([]fwk.NodeInfo, error) {
	return f.havePodsWithAffinityNodeInfoList, nil
}

func (f *fakeSharedLister) HavePodsWithRequiredAntiAffinityList() ([]fwk.NodeInfo, error) {
	return f.havePodsWithRequiredAntiAffinityNodeInfoList, nil
}

func (f *fakeSharedLister) Get(nodeName string) (fwk.NodeInfo, error) {
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
	// the activeQ or unschedulablePods.
	nominatedPods map[string][]fwk.PodInfo
	// nominatedPodToNode is map keyed by a Pod UID to the node name where it is
	// nominated.
	nominatedPodToNode map[types.UID]string

	lock sync.RWMutex
}

// DeleteNominatedPodIfExists deletes <pod> from nominatedPods.
func (npm *nominator) DeleteNominatedPodIfExists(pod *v1.Pod) {
	npm.lock.Lock()
	npm.deleteNominatedPodIfExistsUnlocked(pod)
	npm.lock.Unlock()
}

func (npm *nominator) deleteNominatedPodIfExistsUnlocked(pod *v1.Pod) {
	npm.delete(pod)
}

// AddNominatedPod adds a pod to the nominated pods of the given node.
// This is called during the preemption process after a node is nominated to run
// the pod. We update the structure before sending a request to update the pod
// object to avoid races with the following scheduling cycles.
func (npm *nominator) AddNominatedPod(logger klog.Logger, pi fwk.PodInfo, nominatingInfo *framework.NominatingInfo) {
	npm.lock.Lock()
	npm.addNominatedPodUnlocked(logger, pi, nominatingInfo)
	npm.lock.Unlock()
}

func (npm *nominator) addNominatedPodUnlocked(logger klog.Logger, pi fwk.PodInfo, nominatingInfo *framework.NominatingInfo) {
	// Always delete the pod if it already exists, to ensure we never store more than
	// one instance of the pod.
	npm.delete(pi.GetPod())

	var nodeName string
	if nominatingInfo.Mode() == framework.ModeOverride {
		nodeName = nominatingInfo.NominatedNodeName
	} else if nominatingInfo.Mode() == framework.ModeNoop {
		if pi.GetPod().Status.NominatedNodeName == "" {
			return
		}
		nodeName = pi.GetPod().Status.NominatedNodeName
	}

	if npm.podLister != nil {
		// If the pod was removed or if it was already scheduled, don't nominate it.
		updatedPod, err := npm.podLister.Pods(pi.GetPod().Namespace).Get(pi.GetPod().Name)
		if err != nil {
			logger.V(4).Info("Pod doesn't exist in podLister, aborted adding it to the nominator", "pod", klog.KObj(pi.GetPod()))
			return
		}
		if updatedPod.Spec.NodeName != "" {
			logger.V(4).Info("Pod is already scheduled to a node, aborted adding it to the nominator", "pod", klog.KObj(pi.GetPod()), "node", updatedPod.Spec.NodeName)
			return
		}
	}

	npm.nominatedPodToNode[pi.GetPod().UID] = nodeName
	for _, npi := range npm.nominatedPods[nodeName] {
		if npi.GetPod().UID == pi.GetPod().UID {
			logger.V(4).Info("Pod already exists in the nominator", "pod", klog.KObj(npi.GetPod()))
			return
		}
	}
	npm.nominatedPods[nodeName] = append(npm.nominatedPods[nodeName], pi)
}

func (npm *nominator) delete(p *v1.Pod) {
	nnn, ok := npm.nominatedPodToNode[p.UID]
	if !ok {
		return
	}
	for i, np := range npm.nominatedPods[nnn] {
		if np.GetPod().UID == p.UID {
			npm.nominatedPods[nnn] = append(npm.nominatedPods[nnn][:i], npm.nominatedPods[nnn][i+1:]...)
			if len(npm.nominatedPods[nnn]) == 0 {
				delete(npm.nominatedPods, nnn)
			}
			break
		}
	}
	delete(npm.nominatedPodToNode, p.UID)
}

// UpdateNominatedPod updates the <oldPod> with <newPod>.
func (npm *nominator) UpdateNominatedPod(logger klog.Logger, oldPod *v1.Pod, newPodInfo fwk.PodInfo) {
	npm.lock.Lock()
	defer npm.lock.Unlock()
	npm.updateNominatedPodUnlocked(logger, oldPod, newPodInfo)
}

func (npm *nominator) updateNominatedPodUnlocked(logger klog.Logger, oldPod *v1.Pod, newPodInfo fwk.PodInfo) {
	// In some cases, an Update event with no "NominatedNode" present is received right
	// after a node("NominatedNode") is reserved for this pod in memory.
	// In this case, we need to keep reserving the NominatedNode when updating the pod pointer.
	var nominatingInfo *framework.NominatingInfo
	// We won't fall into below `if` block if the Update event represents:
	// (1) NominatedNode info is added
	// (2) NominatedNode info is updated
	// (3) NominatedNode info is removed
	if NominatedNodeName(oldPod) == "" && NominatedNodeName(newPodInfo.GetPod()) == "" {
		if nnn, ok := npm.nominatedPodToNode[oldPod.UID]; ok {
			// This is the only case we should continue reserving the NominatedNode
			nominatingInfo = &framework.NominatingInfo{
				NominatingMode:    framework.ModeOverride,
				NominatedNodeName: nnn,
			}
		}
	}
	// We update irrespective of the nominatedNodeName changed or not, to ensure
	// that pod pointer is updated.
	npm.delete(oldPod)
	npm.addNominatedPodUnlocked(logger, newPodInfo, nominatingInfo)
}

// NominatedPodsForNode returns a copy of pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node.
func (npm *nominator) NominatedPodsForNode(nodeName string) []fwk.PodInfo {
	npm.lock.RLock()
	defer npm.lock.RUnlock()
	// Make a copy of the nominated Pods so the caller can mutate safely.
	pods := make([]fwk.PodInfo, len(npm.nominatedPods[nodeName]))
	for i := 0; i < len(pods); i++ {
		pods[i] = npm.nominatedPods[nodeName][i]
	}
	return pods
}

// NewPodNominator creates a nominator as a backing of framework.PodNominator.
// A podLister is passed in so as to check if the pod exists
// before adding its nominatedNode info.
func NewPodNominator(podLister listersv1.PodLister) framework.PodNominator {
	return newPodNominator(podLister)
}

func newPodNominator(podLister listersv1.PodLister) *nominator {
	return &nominator{
		podLister:          podLister,
		nominatedPods:      make(map[string][]fwk.PodInfo),
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
func NewFakeClient(objs ...runtime.Object) (client.WithWatch, error) {
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
func NewClientOrDie(ctx context.Context, cfg *rest.Config) client.Client {
	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	c, _, err := util.NewClientWithCachedReader(ctx, cfg, scheme)
	if err != nil {
		panic(err)
	}
	return c
}
