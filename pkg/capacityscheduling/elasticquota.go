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

func (e ElasticQuotaInfos) aggregatedMinOverUsedWithPod(podRequest framework.Resource) bool {
	used := framework.NewResource(nil)
	min := framework.NewResource(nil)

	for _, elasticQuotaInfo := range e {
		used.Add(elasticQuotaInfo.Used.ResourceList())
		min.Add(elasticQuotaInfo.Min.ResourceList())
	}

	used.Add(podRequest.ResourceList())
	return moreThanMin(*used, *min)
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

func (e *ElasticQuotaInfo) overUsed(podRequest framework.Resource, resource *framework.Resource) bool {
	if e.Used.MilliCPU+podRequest.MilliCPU > resource.MilliCPU {
		return true
	}

	if e.Used.Memory+podRequest.Memory > resource.Memory {
		return true
	}

	for rName, rQuant := range podRequest.ScalarResources {
		if rQuant+e.Used.ScalarResources[rName] > resource.ScalarResources[rName] {
			return true
		}
	}
	return false
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
	e.reserveResource(podRequest.Resource)

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
	e.unreserveResource(podRequest.Resource)

	return nil
}

func moreThanMin(used, min framework.Resource) bool {
	if used.MilliCPU > min.MilliCPU {
		return true
	}
	if used.Memory > min.Memory {
		return true
	}

	for rName, rQuant := range used.ScalarResources {
		if rQuant > min.ScalarResources[rName] {
			return true
		}
	}

	return false
}
