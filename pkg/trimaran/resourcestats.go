/*
Copyright 2022 The Kubernetes Authors.

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

package trimaran

import (
	"math"

	"github.com/paypal/load-watcher/pkg/watcher"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// MegaFactor : Mega unit multiplier
	MegaFactor = float64(1. / 1024. / 1024.)
)

// ResourceStats : statistics data for a resource
type ResourceStats struct {
	// average used (absolute)
	UsedAvg float64
	// standard deviation used (absolute)
	UsedStdev float64
	// req of pod
	Req float64
	// node capacity
	Capacity float64
}

// CreateResourceStats : get resource statistics data from measurements for a node
func CreateResourceStats(metrics []watcher.Metric, node *v1.Node, podRequest *framework.Resource,
	resourceName v1.ResourceName, watcherType string) (rs *ResourceStats, isValid bool) {
	// get resource usage statistics
	nodeUtil, nodeStd, metricFound := GetResourceData(metrics, watcherType)
	if !metricFound {
		klog.V(6).InfoS("Resource usage statistics for node : no valid data", "node", klog.KObj(node))
		return nil, false
	}
	// get resource capacity
	rs = &ResourceStats{}
	allocatableResources := node.Status.Allocatable
	am := allocatableResources[resourceName]

	if resourceName == v1.ResourceCPU {
		rs.Capacity = float64(am.MilliValue())
		rs.Req = float64(podRequest.MilliCPU)
	} else {
		rs.Capacity = float64(am.Value())
		rs.Capacity *= MegaFactor
		rs.Req = float64(podRequest.Memory) * MegaFactor
	}

	// calculate absolute usage statistics
	rs.UsedAvg = nodeUtil * rs.Capacity / 100
	rs.UsedStdev = nodeStd * rs.Capacity / 100

	klog.V(6).InfoS("Resource usage statistics for node", "node", klog.KObj(node), "capacity", rs.Capacity, "required", rs.Req, "usedAvg", rs.UsedAvg, "usedStdev", rs.UsedStdev)
	return rs, true
}

// GetMuSigma : get average and standard deviation from statistics
func GetMuSigma(rs *ResourceStats) (float64, float64) {

	if rs.Capacity <= 0 {
		return 0, 0
	}
	mu := (rs.UsedAvg + rs.Req) / rs.Capacity
	mu = math.Max(math.Min(mu, 1), 0)
	sigma := rs.UsedStdev / rs.Capacity
	sigma = math.Max(math.Min(sigma, 1), 0)
	return mu, sigma
}

// GetResourceData : get data from measurements for a given resource type
func GetResourceData(metrics []watcher.Metric, resourceType string) (avg float64, stDev float64, isValid bool) {
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

// GHetResourceRequested : calculate the resource demand of a pod (CPU and Memory)
func GetResourceRequested(pod *v1.Pod) *framework.Resource {
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
