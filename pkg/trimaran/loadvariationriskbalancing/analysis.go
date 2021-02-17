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
	// demand of pod
	demand float64
	// node capacity
	capacity float64
}

// computeScore : compute score given usage statistics
func (rs *resourceStats) computeScore(margin float64) int64 {
	if rs.capacity <= 0 {
		klog.Errorf("invalid resource capacity %f!", rs.capacity)
		return 0
	}
	mu := (rs.usedAvg + rs.demand) / rs.capacity
	mu = math.Max(math.Min(mu, 1), 0)
	sigma := rs.usedStdev / rs.capacity
	sigma *= margin
	sigma = math.Max(math.Min(sigma, 1), 0)
	obj := (mu + math.Sqrt(sigma)) / 2
	klog.V(6).Infof("mu=%f; sigma=%f; margin=%f; obj=%f", mu, sigma, margin, obj)
	objScaled := (1. - obj) * float64(framework.MaxNodeScore)
	score := int64(objScaled + 0.5)
	return score
}

// getCPUStats : get CPU statistics data from measurements for a node
func getCPUStats(metrics []watcher.Metric, node *v1.Node,
	podRequest *framework.Resource) (rs *resourceStats, isValid bool) {
	// get CPU usage statistics
	nodeCPUUtil, nodeCPUStd, cpuMetricFound := getResourceData(metrics, node, watcher.CPU)
	if !cpuMetricFound {
		klog.V(4).Infof("CPU usage statistics for node %s: no valid data", node.GetName())
		return nil, false
	}
	// get CPU capacity
	rs = &resourceStats{}
	allocatableResources := node.Status.Allocatable
	if am := allocatableResources["cpu"]; &am != nil {
		rs.capacity = float64((&am).MilliValue())
	}
	// calculate absolute usage statistics (in millicores)
	rs.usedAvg = nodeCPUUtil * rs.capacity / 100
	rs.usedStdev = nodeCPUStd * rs.capacity / 100
	rs.demand = float64(podRequest.MilliCPU)
	klog.V(4).Infof("CPU usage statistics for node %s: capacity=%f; demand=%f; usedAvg=%f; usedStdev=%f",
		node.GetName(), rs.capacity, rs.demand, rs.usedAvg, rs.usedStdev)
	return rs, true
}

// getMemoryStats : get memory statistics data from measurements for a node
func getMemoryStats(metrics []watcher.Metric, node *v1.Node,
	podRequest *framework.Resource) (rs *resourceStats, isValid bool) {
	// get memory usage statistics
	nodeMemoryUtil, nodeMemoryStd, memoryMetricFound := getResourceData(metrics, node, watcher.Memory)
	if !memoryMetricFound {
		klog.V(4).Infof("Memory usage statistics for node %s: no valid data", node.GetName())
		return nil, false
	}
	// get memory capacity
	rs = &resourceStats{}
	allocatableResources := node.Status.Allocatable
	if am := allocatableResources["memory"]; &am != nil {
		rs.capacity = float64((&am).Value())
	}
	var megaFactor = float64(1. / 1024. / 1024.)
	rs.capacity *= megaFactor
	// calculate absolute usage statistics (in MB)
	rs.usedAvg = nodeMemoryUtil * rs.capacity / 100
	rs.usedStdev = nodeMemoryStd * rs.capacity / 100
	rs.demand = float64(podRequest.Memory) * megaFactor
	klog.V(4).Infof("Memory usage statistics for node %s: capacity=%f MB; demand=%f MB; usedAvg=%f MB; usedStdev=%f MB",
		node.GetName(), rs.capacity, rs.demand, rs.usedAvg, rs.usedStdev)
	return rs, true
}

// getResourceData : get data from measurements for a node for a given resource type
func getResourceData(metrics []watcher.Metric, node *v1.Node, resourceType string) (avg float64, stDev float64, isValid bool) {
	avg = 0
	stDev = 0
	isValid = false
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
			} else if metric.Operator == "" && !avgFound {
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
				if CPU := rQuantity.MilliValue(); CPU > result.MilliCPU {
					result.MilliCPU = CPU
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
