/*
Copyright 2025 The Kubernetes Authors.

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

package resourcepolicy

import (
	"fmt"
	"sync"

	"github.com/KunWuLuan/resourcepolicyapi/pkg/apis/scheduling/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/ptr"
)

type resourcePolicyCache struct {
	processingLock sync.RWMutex

	// rps is map, keyed with namespace
	// value is map, keyed with NamespaceName of ResourcePolicy
	rps map[string]map[keyStr]*resourcePolicyInfo
	// pd2Rps stores all pods that have been assumed
	// pd2Rps is map, keyed with namespaceName of pod
	// value is namespaceName of ResourcePolicy
	pd2Rps         map[keyStr]keyStr
	assumedPd2Node map[keyStr]string

	wq workqueue.TypedRateLimitingInterface[types.NamespacedName]

	nl v1.NodeLister
	pl v1.PodLister
	ss framework.NodeInfoLister
}

func NewResourcePolicyCache(
	nl v1.NodeLister,
	pl v1.PodLister,
	ss framework.NodeInfoLister,
) *resourcePolicyCache {
	cache := &resourcePolicyCache{
		rps:    make(map[string]map[keyStr]*resourcePolicyInfo),
		pd2Rps: make(map[keyStr]keyStr),

		wq: workqueue.NewTypedRateLimitingQueueWithConfig[types.NamespacedName](
			workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName](),
			workqueue.TypedRateLimitingQueueConfig[types.NamespacedName]{Name: "resourcepolicy"}),

		nl: nl,
		pl: pl,
		ss: ss,
	}
	go cache.updateLoop()
	return cache
}

// get the lock outside
func (rspc *resourcePolicyCache) getResourcePolicyInfoByKey(key keyStr, namespace string) *resourcePolicyInfo {
	rpsInNs := rspc.rps[namespace]
	if len(rpsInNs) == 0 {
		return nil
	}
	return rpsInNs[key]
}

func (rspc *resourcePolicyCache) Assume(cycleState *framework.CycleState, pod *corev1.Pod, node *corev1.Node) error {
	state, err := cycleState.Read(ResourcePolicyPreFilterStateKey)
	if err != nil {
		return nil
	}
	preFilterState, ok := state.(*ResourcePolicyPreFilterState)
	if !ok {
		return fmt.Errorf("unable to convert state to ResourcePolicyPreFilterState")
	}

	rspc.processingLock.Lock()
	defer rspc.processingLock.Unlock()

	podKey := GetKeyStr(pod.ObjectMeta)
	if node, ok := rspc.assumedPd2Node[podKey]; ok {
		return fmt.Errorf("PodAlreadyAssumed assumed node: %v", node)
	}
	rspc.pd2Rps[podKey] = preFilterState.matchedInfo.ks

	var unitIndex *int
	rspi := preFilterState.matchedInfo
	for idx, sel := range rspi.nodeSelectors {
		if !sel.Matches(labels.Set(node.Labels)) {
			continue
		}
		if unitIndex == nil {
			unitIndex = ptr.To(idx)
		}
		rspi.addPodToBoundOrAssumedPods(rspi.assumedPods, idx, preFilterState.labelKeyValue, node.Name, podKey, preFilterState.podRes)
	}
	if unitIndex != nil {
		preFilterState.assumedUnitIndex = *unitIndex
	}
	return nil
}

func (rspc *resourcePolicyCache) Forget(cycleState *framework.CycleState, pod *corev1.Pod, node *corev1.Node) {
	state, err := cycleState.Read(ResourcePolicyPreFilterStateKey)
	if err != nil {
		return
	}
	preFilterState, ok := state.(*ResourcePolicyPreFilterState)
	if !ok {
		return
	}

	rspc.processingLock.Lock()
	defer rspc.processingLock.Unlock()

	podKey := GetKeyStr(pod.ObjectMeta)
	delete(rspc.assumedPd2Node, podKey)

	rspi := preFilterState.matchedInfo
	for idx, sel := range rspi.nodeSelectors {
		if !sel.Matches(labels.Set(node.Labels)) {
			continue
		}
		rspi.removePodFromBoundOrAssumedPods(rspi.assumedPods, idx, preFilterState.labelKeyValue, node.Name, podKey)
	}
}

// need to obtain lock outside
func (rspc *resourcePolicyCache) AddOrUpdateBoundPod(p *corev1.Pod) {
	podKey := GetKeyStr(p.ObjectMeta)
	rspkey := GetManagedResourcePolicy(p)
	assumedRspKey, ok := rspc.pd2Rps[podKey]
	if ok && assumedRspKey != rspkey {
		klog.ErrorS(fmt.Errorf("AssignedResourcePolicyNotMatch"), "bound pod is managed by another resourcepolicy", "assumed", assumedRspKey, "bound", rspkey)
		delete(rspc.pd2Rps, podKey)

		rsps, ok := rspc.rps[p.Namespace]
		if ok {
			rspi, ok := rsps[assumedRspKey]
			if ok {
				valid, labelKeyValue := genLabelKeyValueForPod(rspi.policy, p)
				if valid {
					rspi.processingLock.Lock()
					rspi.removePod(podKey, labelKeyValue)
					rspi.processingLock.Unlock()
				}
			}
		}
	}

	boundRsp := rspc.getResourcePolicyInfoByKey(rspkey, p.Namespace)
	if boundRsp == nil {
		klog.ErrorS(fmt.Errorf("ResourcePolicyInfoNotFound"), "bound pod is managed by a resourcepolicy that is not found", "bound", rspkey)
		return
	}

	nodeName := p.Spec.NodeName
	if nodeName == "" {
		klog.ErrorS(fmt.Errorf("PodNotBound"), "pod is not bound", "pod", klog.KObj(p))
		return
	}

	node, err := rspc.nl.Get(nodeName)
	if err != nil {
		klog.ErrorS(err, "failed to get node", "node", nodeName)
		return
	}

	valid, labelKeyValue := genLabelKeyValueForPod(boundRsp.policy, p)
	if !valid {
		return
	}
	boundRsp.processingLock.Lock()
	defer boundRsp.processingLock.Unlock()
	podRes := resourcehelper.PodRequests(p, resourcehelper.PodResourcesOptions{})
	for idx, sel := range boundRsp.nodeSelectors {
		if !sel.Matches(labels.Set(node.Labels)) {
			// TODO: remove pod from this count
			// do not remove node from unit by update node values
			continue
		}
		boundRsp.removePodFromBoundOrAssumedPods(boundRsp.assumedPods, idx, labelKeyValue, node.Name, podKey)
		boundRsp.addPodToBoundOrAssumedPods(boundRsp.boundPods, idx, labelKeyValue, nodeName, podKey, framework.NewResource(podRes))
	}
}

func (rspc *resourcePolicyCache) DeleteBoundPod(p *corev1.Pod) {
	rspc.processingLock.Lock()
	defer rspc.processingLock.Unlock()

	podKey := GetKeyStr(p.ObjectMeta)
	rspkey := GetManagedResourcePolicy(p)
	rsp := rspc.getResourcePolicyInfoByKey(rspkey, p.Namespace)
	if rsp != nil {
		valid, labelKeyValue := genLabelKeyValueForPod(rsp.policy, p)
		if valid {
			rsp.removePod(podKey, labelKeyValue)
		}
	}

	assumedRspKey := rspc.pd2Rps[podKey]
	assumedRsp := rspc.getResourcePolicyInfoByKey(assumedRspKey, p.Namespace)
	if assumedRsp != nil {
		valid, labelKeyValue := genLabelKeyValueForPod(assumedRsp.policy, p)
		if valid {
			assumedRsp.removePod(podKey, labelKeyValue)
		}
	}
	delete(rspc.pd2Rps, podKey)
	delete(rspc.assumedPd2Node, podKey)
}

func (rspc *resourcePolicyCache) DeleteResourcePolicy(rsp *v1alpha1.ResourcePolicy) {
	rspc.processingLock.Lock()
	defer rspc.processingLock.Unlock()

	ns := rsp.Namespace
	rspKey := GetKeyStr(rsp.ObjectMeta)

	if rspInNs, ok := rspc.rps[ns]; ok {
		if _, rspok := rspInNs[rspKey]; rspok {
			delete(rspInNs, rspKey)
		}
		if len(rspInNs) == 0 {
			delete(rspc.rps, ns)
		}
	}
}

func (rspc *resourcePolicyCache) AddOrUpdateResPolicy(rsp *v1alpha1.ResourcePolicy) {
	rspc.processingLock.Lock()
	defer rspc.processingLock.Unlock()

	ns := rsp.Namespace
	rspKey := GetKeyStr(rsp.ObjectMeta)

	if rspInNs, ok := rspc.rps[ns]; ok {
		if rspinfo, rspok := rspInNs[rspKey]; rspok {
			if rspinfo.rv == rsp.ResourceVersion {
				return
			}

			rspinfo.processingLock.Lock()
			rspinfo.processing = true

			rspinfo.rv = rsp.ResourceVersion
			rspinfo.policy = rsp
			rspinfo.podSelector = labels.SelectorFromSet(rsp.Spec.Selector)
			rspinfo.processingLock.Unlock()

			rspc.wq.AddRateLimited(types.NamespacedName{Namespace: rsp.Namespace, Name: rsp.Name})
			return
		}
	} else {
		rspc.rps[ns] = make(map[keyStr]*resourcePolicyInfo)
	}

	newRspInfo := newResourcePolicyInfo()
	newRspInfo.processingLock.Lock()
	newRspInfo.processing = true
	rspc.rps[ns][rspKey] = newRspInfo

	newRspInfo.ks = rspKey
	newRspInfo.rv = rsp.ResourceVersion
	newRspInfo.policy = rsp
	newRspInfo.podSelector = labels.SelectorFromSet(rsp.Spec.Selector)
	newRspInfo.processingLock.Unlock()

	rspc.wq.AddRateLimited(types.NamespacedName{Namespace: rsp.Namespace, Name: rsp.Name})
}

func (rspc *resourcePolicyCache) updateLoop() {
	for {
		item, shutdown := rspc.wq.Get()
		if shutdown {
			return
		}

		rspc.processingLock.RLock()
		ns := item.Namespace
		rspKey := item.Namespace + "/" + item.Name

		var ok bool
		var rspInNs map[keyStr]*resourcePolicyInfo
		var rspinfo *resourcePolicyInfo
		if rspInNs, ok = rspc.rps[ns]; !ok {
			rspc.wq.Done(item)
			rspc.processingLock.RUnlock()
			continue
		}
		if rspinfo, ok = rspInNs[keyStr(rspKey)]; !ok {
			rspc.wq.Done(item)
			rspc.processingLock.RUnlock()
			continue
		}
		rspc.processingLock.RUnlock()

		rspinfo.complete(rspc.pl, rspc.nl, rspc.ss, rspc.pd2Rps)
	}
}
