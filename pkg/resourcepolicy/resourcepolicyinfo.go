package resourcepolicy

import (
	"fmt"
	"sync"

	"github.com/KunWuLuan/resourcepolicyapi/pkg/apis/scheduling/v1alpha1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type resourcePolicyInfo struct {
	processingLock sync.Mutex
	// when processing is ture, means cache of the rspinfo is waiting for reconcile
	// do not schedule pod in this rsp.
	processing bool

	ks keyStr
	rv string

	policy        *v1alpha1.ResourcePolicy
	podSelector   labels.Selector
	nodeSelectors []labels.Selector
	//
	// [unit.index] {
	// 	"node1": {
	// 		"/": [ pod1.ns/name, ... ],
	// 		"matchLabelKeys": [ pod2.ns/name, ... ],
	// 	}
	// }
	assumedPods multiLevelPodSet
	boundPods   multiLevelPodSet
	// contains assumed and bound pods
	// [unit.index] {
	//   "/": 1
	//   "matchLabelKeys-v1": 1
	// }
	assumedPodCount []map[labelKeysValue]int
	//
	// key: pod.Namespace/pod.Name
	podResourceDetails    map[keyStr]*framework.Resource
	maxPodResources       []framework.Resource
	assumedPodConsumption []map[labelKeysValue]*framework.Resource
}

func newResourcePolicyInfo() *resourcePolicyInfo {
	return &resourcePolicyInfo{
		assumedPods:           make(multiLevelPodSet, 0),
		boundPods:             make(multiLevelPodSet, 0),
		assumedPodCount:       make([]map[labelKeysValue]int, 0),
		podResourceDetails:    make(map[keyStr]*framework.Resource),
		maxPodResources:       make([]framework.Resource, 0),
		assumedPodConsumption: make([]map[labelKeysValue]*framework.Resource, 0),
	}
}

func (rspi *resourcePolicyInfo) complete(pl listersv1.PodLister, nl listersv1.NodeLister, ss framework.NodeInfoLister,
	assumedPods map[keyStr]keyStr) {
	if !rspi.processing {
		return
	}
	if rspi.policy == nil {
		klog.ErrorS(fmt.Errorf("ResourcePolicyInfoNotInited"), "resourcePolicyInfo not inited", "rspKey", rspi.ks)
		return
	}

	rspi.processingLock.Lock()
	defer func() {
		// resourcepolicy has been initialized, pods can be scheduled again
		rspi.processing = false
		rspi.processingLock.Unlock()
	}()

	rspi.nodeSelectors = make([]labels.Selector, len(rspi.policy.Spec.Units))
	for idx, unit := range rspi.policy.Spec.Units {
		selector := labels.NewSelector()
		reqs, _ := labels.SelectorFromSet(unit.NodeSelector.MatchLabels).Requirements()
		selector.Add(reqs...)
		for _, exp := range unit.NodeSelector.MatchExpressions {
			req, err := labels.NewRequirement(exp.Key, selection.Operator(exp.Operator), exp.Values)
			if err != nil {
				continue
			}
			selector.Add(*req)
		}
		rspi.nodeSelectors[idx] = selector

		nodes, err := nl.List(selector)
		if err != nil {
			continue
		}
		for _, no := range nodes {
			ni, err := ss.Get(no.Name)
			if err != nil {
				continue
			}
			for _, po := range ni.Pods {
				if GetManagedResourcePolicy(po.Pod) != rspi.ks &&
					assumedPods[GetKeyStr(po.Pod.ObjectMeta)] != rspi.ks {
					continue
				}
				ok, labelKeyValue := genLabelKeyValueForPod(rspi.policy, po.Pod)
				if !ok {
					continue
				}
				res := resource.PodRequests(po.Pod, resource.PodResourcesOptions{})
				rspi.addPodToBoundOrAssumedPods(rspi.boundPods, idx, labelKeyValue, no.Name, GetKeyStr(po.Pod.ObjectMeta), framework.NewResource(res))
			}
		}
	}
}

// lock should be get outside
// TODO: when pod resource is chanaged, assumedPodConsumption maybe wrong
func (r *resourcePolicyInfo) addPodToBoundOrAssumedPods(ps multiLevelPodSet, index int, labelValues labelKeysValue, nodename string, podKey keyStr, res *framework.Resource) bool {
	podsetByValue := ps[index][nodename]
	if podsetByValue == nil {
		podsetByValue = make(map[labelKeysValue]sets.Set[keyStr])
	}
	podset := podsetByValue[labelValues]
	if podset == nil {
		podset = sets.New[keyStr]()
	}
	if podset.Has(podKey) {
		return false
	}
	podset.Insert(podKey)
	podsetByValue[labelValues] = podset
	ps[index][nodename] = podsetByValue
	if curRes, ok := r.podResourceDetails[podKey]; !ok || !equalResource(res, curRes) {
		r.podResourceDetails[podKey] = res
	}
	r.updateAssumedPodCount(index, labelValues, 1, res)
	return true
}

// TODO: handle the case that pod's labelKeyValue was changed
func (r *resourcePolicyInfo) removePod(podKeyStr keyStr, labelKeyValue labelKeysValue) {
	for _, podSetByNode := range r.boundPods {
		for _, podSetByLabelValues := range podSetByNode {
			for _, podSet := range podSetByLabelValues {
				podSet.Delete(podKeyStr)
			}
		}
	}
	for _, podSetByNode := range r.assumedPods {
		for _, podSetByLabelValues := range podSetByNode {
			for _, podSet := range podSetByLabelValues {
				podSet.Delete(podKeyStr)
			}
		}
	}

	podRes := r.podResourceDetails[podKeyStr]
	if podRes != nil {
		for idx := range r.assumedPodConsumption {
			r.updateAssumedPodResource(idx, labelKeyValue, false, podRes)
		}
		delete(r.podResourceDetails, podKeyStr)
	}
}

// lock should be get outside
func (r *resourcePolicyInfo) removePodFromBoundOrAssumedPods(ps multiLevelPodSet, index int, labelValues labelKeysValue, nodename string, podKey keyStr) (bool, error) {
	podsetByValue := ps[index][nodename]
	if podsetByValue == nil {
		return false, nil // fmt.Errorf("labelValues %v not found", labelValues)
	}
	podset := podsetByValue[labelValues]
	if podset == nil {
		return false, nil // fmt.Errorf("node %v not found", nodename)
	}
	if !podset.Has(podKey) {
		return false, fmt.Errorf("pod %v not found", podKey)
	}
	podset.Delete(podKey)
	if podset.Len() == 0 {
		delete(podsetByValue, labelValues)
	} else {
		podsetByValue[labelValues] = podset
	}
	ps[index][nodename] = podsetByValue

	res := framework.NewResource(nil)
	if detail, ok := r.podResourceDetails[podKey]; ok {
		res = detail
	}
	if err := r.updateAssumedPodCount(index, labelValues, -1, res); err != nil {
		return false, err
	}
	return true, nil
}

func (r *resourcePolicyInfo) updateAssumedPodCount(index int, v labelKeysValue, count int, totalRes *framework.Resource) error {
	if count > 0 {
		if r.assumedPodCount[index] == nil {
			r.assumedPodCount[index] = map[labelKeysValue]int{v: count}
		} else {
			r.assumedPodCount[index][v] += count
		}
		r.updateAssumedPodResource(index, v, true, totalRes)
	} else {
		if r.assumedPodCount[index] == nil {
			return fmt.Errorf("pod not found in assumedPodCount, this should never happen")
		} else {
			r.assumedPodCount[index][v] += count
			if r.assumedPodCount[index][v] == 0 {
				delete(r.assumedPodCount[index], v)
			}
		}
		r.updateAssumedPodResource(index, v, false, totalRes)
	}
	return nil
}

func (r *resourcePolicyInfo) updateAssumedPodResource(index int, v labelKeysValue, add bool, totalRes *framework.Resource) {
	if add {
		if r.assumedPodConsumption[index] == nil {
			r.assumedPodConsumption[index] = map[labelKeysValue]*framework.Resource{v: totalRes.Clone()}
		} else {
			addResources(r.assumedPodConsumption[index][v], totalRes)
		}
	} else {
		if r.assumedPodConsumption[index] == nil {
			return
		} else {
			if r.assumedPodConsumption[index][v] == nil {
				return
			}
			subResources(r.assumedPodConsumption[index][v], totalRes)
		}
	}
}
