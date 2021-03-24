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

package loadvariationriskbalancing

import (
	"math"

	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
)

/*
Calculation of risk score for resources given measured data
*/

// resourceStats : statistics data for a resource
type resourceStats struct {
	// average used (absolute)
	usedAvg float64
	// standard deviation used (absolute)
	usedStdev float64
	// req of pod
	req float64
	// node capacity
	capacity float64
}

// computeScore : compute score given usage statistics
// - risk = [ average + margin * stDev^{1/sensitivity} ] / 2
// - score = ( 1 - risk ) * maxScore
func (rs *resourceStats) computeScore(margin float64, sensitivity float64) float64 {
	if rs.capacity <= 0 {
		klog.Errorf("invalid resource capacity %f!", rs.capacity)
		return 0
	}

	// make sure values are within bounds
	rs.req = math.Max(rs.req, 0)
	rs.usedAvg = math.Max(math.Min(rs.usedAvg, rs.capacity), 0)
	rs.usedStdev = math.Max(math.Min(rs.usedStdev, rs.capacity), 0)

	// calculate average factor
	mu := (rs.usedAvg + rs.req) / rs.capacity
	mu = math.Max(math.Min(mu, 1), 0)

	// calculate deviation factor
	sigma := rs.usedStdev / rs.capacity
	sigma = math.Max(math.Min(sigma, 1), 0)
	// apply root power
	if sensitivity >= 0 {
		sigma = math.Pow(sigma, 1/sensitivity)
	}
	// apply multiplier
	sigma *= margin
	sigma = math.Max(math.Min(sigma, 1), 0)

	// evaluate overall risk factor
	risk := (mu + sigma) / 2
	klog.V(6).Infof("mu=%f; sigma=%f; margin=%f; sensitivity=%f; risk=%f", mu, sigma, margin, sensitivity, risk)
	return (1. - risk) * float64(framework.MaxNodeScore)
}

// createResourceStats : get resource statistics data from measurements for a node
func createResourceStats(metrics []watcher.Metric, node *v1.Node, podRequest *framework.Resource,
	resourceName v1.ResourceName, watcherType string) (rs *resourceStats, isValid bool) {
	// get resource usage statistics
	nodeUtil, nodeStd, metricFound := getResourceData(metrics, watcherType)
	if !metricFound {
		klog.V(6).Infof("resource %s usage statistics for node %s: no valid data", watcherType, node.GetName())
		return nil, false
	}
	// get resource capacity
	rs = &resourceStats{}
	allocatableResources := node.Status.Allocatable
	am := allocatableResources[resourceName]

	if resourceName == v1.ResourceCPU {
		rs.capacity = float64(am.MilliValue())
		rs.req = float64(podRequest.MilliCPU)
	} else {
		rs.capacity = float64(am.Value())
		var megaFactor = float64(1. / 1024. / 1024.)
		rs.capacity *= megaFactor
		rs.req = float64(podRequest.Memory) * megaFactor
	}

	// calculate absolute usage statistics
	rs.usedAvg = nodeUtil * rs.capacity / 100
	rs.usedStdev = nodeStd * rs.capacity / 100

	klog.V(6).Infof("resource %s usage statistics for node %s: capacity=%f; req=%f; usedAvg=%f; usedStdev=%f",
		watcherType, node.GetName(), rs.capacity, rs.req, rs.usedAvg, rs.usedStdev)
	return rs, true
}

// getResourceData : get data from measurements for a given resource type
func getResourceData(metrics []watcher.Metric, resourceType string) (avg float64, stDev float64, isValid bool) {
	// for backward compatibility of LoadWatcher:
	// average data metric without operator specified
	avgFound := false
	for _, metric := range metrics {
		if metric.Type == resourceType {
			if metric.Operator == watcher.Average {
				avg = metric.Value
				avgFound = true
			} else if metric.Operator == watcher.Std {
				stDev = metric.Value
			} else if (metric.Operator == "" || metric.Operator == watcher.Latest) && !avgFound {
				avg = metric.Value
			}
			isValid = true
		}
	}
	return avg, stDev, isValid
}

// getResourceRequested : calculate the resource demand of a pod (CPU and Memory)
func getResourceRequested(pod *v1.Pod) *framework.Resource {
	// add up demand of all containers
	result := &framework.Resource{}
	for _, container := range pod.Spec.Containers {
		result.Add(container.Resources.Requests)
	}
	// take max_resource(sum_pod, any_init_container)
	for _, container := range pod.Spec.InitContainers {
		for rName, rQuantity := range container.Resources.Requests {
			switch rName {
			case v1.ResourceCPU:
				if cpu := rQuantity.MilliValue(); cpu > result.MilliCPU {
					result.MilliCPU = cpu
				}
			case v1.ResourceMemory:
				if mem := rQuantity.Value(); mem > result.Memory {
					result.Memory = mem
				}
			default:
			}
		}
	}
	// add any pod overhead
	if pod.Spec.Overhead != nil {
		result.Add(pod.Spec.Overhead)
	}
	return result
}
