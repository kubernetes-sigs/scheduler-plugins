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
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/framework"
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
		used.Add(elasticQuotaInfo.Used.ResourceList())
		min.Add(elasticQuotaInfo.Min.ResourceList())
	}

	used.Add(podRequest.ResourceList())
	return cmp(used, min)
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
	for name, value := range request.ScalarResources {
		e.Used.SetScalar(name, e.Used.ScalarResources[name]+value)
	}
}

func (e *ElasticQuotaInfo) unreserveResource(request framework.Resource) {
	e.Used.Memory -= request.Memory
	e.Used.MilliCPU -= request.MilliCPU
	for name, value := range request.ScalarResources {
		e.Used.SetScalar(name, e.Used.ScalarResources[name]-value)
	}
}

func (e *ElasticQuotaInfo) usedOverMinWith(podRequest *framework.Resource) bool {
	return cmp2(podRequest, e.Used, e.Min)
}

func (e *ElasticQuotaInfo) usedOverMaxWith(podRequest *framework.Resource) bool {
	return cmp2(podRequest, e.Used, e.Max)
}

func (e *ElasticQuotaInfo) usedOverMin() bool {
	return cmp(e.Used, e.Min)
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

func cmp(x, y *framework.Resource) bool {
	return cmp2(x, &framework.Resource{}, y)
}

func cmp2(x1, x2, y *framework.Resource) bool {
	if x1.MilliCPU+x2.MilliCPU > y.MilliCPU {
		return true
	}

	if x1.Memory+x2.Memory > y.Memory {
		return true
	}

	for rName, rQuant := range x1.ScalarResources {
		if rQuant+x2.ScalarResources[rName] > y.ScalarResources[rName] {
			return true
		}
	}

	return false
}
