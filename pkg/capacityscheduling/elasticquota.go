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

package capacityscheduling

import (
	"math"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

const (
	UpperBoundOfMax = math.MaxInt64
	LowerBoundOfMin = 0
)

type ElasticQuotaInfos map[string]*ElasticQuotaInfo

func NewElasticQuotaInfos() ElasticQuotaInfos {
	return make(ElasticQuotaInfos)
}

func (e ElasticQuotaInfos) clone() ElasticQuotaInfos {
	elasticQuotas := make(ElasticQuotaInfos)
	for key, elasticQuotaInfo := range e {
		elasticQuotas[key] = elasticQuotaInfo.clone()
	}
	return elasticQuotas
}

func (e ElasticQuotaInfos) aggregatedUsedOverMinWith(podRequest framework.Resource) bool {
	used := framework.NewResource(nil)
	min := framework.NewResource(nil)

	for _, elasticQuotaInfo := range e {
		used.Add(util.ResourceList(elasticQuotaInfo.Used))
		min.Add(util.ResourceList(elasticQuotaInfo.Min))
	}

	used.Add(util.ResourceList(&podRequest))
	return cmp(used, min, LowerBoundOfMin)
}

// ElasticQuotaInfo is a wrapper to a ElasticQuota with information.
// Each namespace can only have one ElasticQuota.
type ElasticQuotaInfo struct {
	Namespace string
	pods      sets.String
	Min       *framework.Resource
	Max       *framework.Resource
	Used      *framework.Resource
}

func newElasticQuotaInfo(namespace string, min, max, used v1.ResourceList) *ElasticQuotaInfo {
	if min == nil {
		min = makeResourceListForBound(LowerBoundOfMin)
	}
	if max == nil {
		max = makeResourceListForBound(UpperBoundOfMax)
	}

	elasticQuotaInfo := &ElasticQuotaInfo{
		Namespace: namespace,
		pods:      sets.NewString(),
		Min:       framework.NewResource(min),
		Max:       framework.NewResource(max),
		Used:      framework.NewResource(used),
	}
	return elasticQuotaInfo
}

func (e *ElasticQuotaInfo) reserveResource(request framework.Resource) {
	e.Used.Memory += request.Memory
	e.Used.MilliCPU += request.MilliCPU
	e.Used.EphemeralStorage += request.EphemeralStorage
	e.Used.AllowedPodNumber += request.AllowedPodNumber
	for name, value := range request.ScalarResources {
		e.Used.SetScalar(name, e.Used.ScalarResources[name]+value)
	}
}

func (e *ElasticQuotaInfo) unreserveResource(request framework.Resource) {
	e.Used.Memory -= request.Memory
	e.Used.MilliCPU -= request.MilliCPU
	e.Used.EphemeralStorage -= request.EphemeralStorage
	e.Used.AllowedPodNumber -= request.AllowedPodNumber
	for name, value := range request.ScalarResources {
		e.Used.SetScalar(name, e.Used.ScalarResources[name]-value)
	}
}

func (e *ElasticQuotaInfo) usedOverMinWith(podRequest *framework.Resource) bool {
	// "ElasticQuotaInfo doesn't have Min" means used values exceeded min(0)
	if e.Min == nil {
		return true
	}
	return cmp2(podRequest, e.Used, e.Min, LowerBoundOfMin)
}

func (e *ElasticQuotaInfo) usedOverMaxWith(podRequest *framework.Resource) bool {
	// "ElasticQuotaInfo doesn't have Max" means there are no limitations(infinite)
	if e.Max == nil {
		return false
	}
	return cmp2(podRequest, e.Used, e.Max, UpperBoundOfMax)
}

func (e *ElasticQuotaInfo) usedOverMin() bool {
	// "ElasticQuotaInfo doesn't have Min" means used values exceeded min(0)
	if e.Min == nil {
		return true
	}
	return cmp(e.Used, e.Min, LowerBoundOfMin)
}

func (e *ElasticQuotaInfo) clone() *ElasticQuotaInfo {
	newEQInfo := &ElasticQuotaInfo{
		Namespace: e.Namespace,
		pods:      sets.NewString(),
	}

	if e.Min != nil {
		newEQInfo.Min = e.Min.Clone()
	}
	if e.Max != nil {
		newEQInfo.Max = e.Max.Clone()
	}
	if e.Used != nil {
		newEQInfo.Used = e.Used.Clone()
	}
	if len(e.pods) > 0 {
		pods := e.pods.List()
		for _, pod := range pods {
			newEQInfo.pods.Insert(pod)
		}
	}

	return newEQInfo
}

func (e *ElasticQuotaInfo) addPodIfNotPresent(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	if e.pods.Has(key) {
		return nil
	}

	e.pods.Insert(key)
	podRequest := computePodResourceRequest(pod)
	e.reserveResource(*podRequest)

	return nil
}

func (e *ElasticQuotaInfo) deletePodIfPresent(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	if !e.pods.Has(key) {
		return nil
	}

	e.pods.Delete(key)
	podRequest := computePodResourceRequest(pod)
	e.unreserveResource(*podRequest)

	return nil
}

func cmp(x, y *framework.Resource, bound int64) bool {
	return cmp2(x, &framework.Resource{}, y, bound)
}

func cmp2(x1, x2, y *framework.Resource, bound int64) bool {
	if x1.MilliCPU+x2.MilliCPU > y.MilliCPU {
		return true
	}

	if x1.Memory+x2.Memory > y.Memory {
		return true
	}

	if x1.EphemeralStorage+x2.EphemeralStorage > y.EphemeralStorage {
		return true
	}

	if x1.AllowedPodNumber+x2.AllowedPodNumber > y.AllowedPodNumber {
		return true
	}

	for rName, rQuant := range x1.ScalarResources {
		yQuant := bound
		if yq, ok := y.ScalarResources[rName]; ok {
			yQuant = yq
		}
		if rQuant+x2.ScalarResources[rName] > yQuant {
			return true
		}
	}

	return false
}

func makeResourceListForBound(bound int64) v1.ResourceList {
	return v1.ResourceList{
		v1.ResourceCPU:              *resource.NewMilliQuantity(bound, resource.DecimalSI),
		v1.ResourceMemory:           *resource.NewQuantity(bound, resource.BinarySI),
		v1.ResourceEphemeralStorage: *resource.NewQuantity(bound, resource.BinarySI),
	}
}
