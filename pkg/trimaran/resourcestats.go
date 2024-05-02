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

	klog.V(6).InfoS("Resource usage statistics for node", "node", klog.KObj(node), "resource", resourceName,
		"capacity", rs.Capacity, "required", rs.Req, "usedAvg", rs.UsedAvg, "usedStdev", rs.UsedStdev)
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

// GetResourceRequested : calculate the resource requests of a pod (CPU and Memory)
func GetResourceRequested(pod *v1.Pod) *framework.Resource {
	return GetEffectiveResource(pod, func(container *v1.Container) v1.ResourceList {
		return container.Resources.Requests
	})
}

// GetResourceLimits : calculate the resource limits of a pod (CPU and Memory)
func GetResourceLimits(pod *v1.Pod) *framework.Resource {
	return GetEffectiveResource(pod, func(container *v1.Container) v1.ResourceList {
		return container.Resources.Limits
	})
}

// GetEffectiveResource: calculate effective resources of a pod (CPU and Memory)
func GetEffectiveResource(pod *v1.Pod, fn func(container *v1.Container) v1.ResourceList) *framework.Resource {
	result := &framework.Resource{}
	// add up resources of all containers
	for _, container := range pod.Spec.Containers {
		result.Add(fn(&container))
	}
	// take max(sum_pod, any_init_container)
	for _, container := range pod.Spec.InitContainers {
		for rName, rQuantity := range fn(&container) {
			switch rName {
			case v1.ResourceCPU:
				setMax(&result.MilliCPU, rQuantity.MilliValue())
			case v1.ResourceMemory:
				setMax(&result.Memory, rQuantity.Value())
			}
		}
	}
	// add any pod overhead
	if pod.Spec.Overhead != nil {
		result.Add(pod.Spec.Overhead)
	}
	return result
}

// NodeRequestsAndLimits : data ralated to requests and limits of resources on a node
type NodeRequestsAndLimits struct {
	// NodeRequest sum of requests of all pods on node
	NodeRequest *framework.Resource
	// NodeLimit sum of limits of all pods on node
	NodeLimit *framework.Resource
	// NodeRequestMinusPod is the NodeRequest without the requests of the pending pod
	NodeRequestMinusPod *framework.Resource
	// NodeLimitMinusPod is the NodeLimit without the limits of the pending pod
	NodeLimitMinusPod *framework.Resource
	// Nodecapacity is the capacity (allocatable) of node
	Nodecapacity *framework.Resource
}

// GetNodeRequestsAndLimits : total requested and limits of resources on a given node plus a pod
func GetNodeRequestsAndLimits(podInfosOnNode []*framework.PodInfo, node *v1.Node, pod *v1.Pod,
	podRequests *framework.Resource, podLimits *framework.Resource) *NodeRequestsAndLimits {
	// initialization
	nodeRequest := &framework.Resource{}
	nodeLimit := &framework.Resource{}
	nodeRequestMinusPod := &framework.Resource{}
	nodeLimitMinusPod := &framework.Resource{}
	// set capacities
	nodeCapacity := &framework.Resource{}
	allocatableResources := node.Status.Allocatable
	amCpu := allocatableResources[v1.ResourceCPU]
	capCpu := amCpu.MilliValue()
	amMem := allocatableResources[v1.ResourceMemory]
	capMem := amMem.Value()
	nodeCapacity.MilliCPU = capCpu
	nodeCapacity.Memory = capMem
	// get requests and limits for all pods
	podsOnNode := make([]*v1.Pod, len(podInfosOnNode))
	for i, pf := range podInfosOnNode {
		podsOnNode[i] = pf.Pod
	}
	for _, p := range append(podsOnNode, pod) {
		var requested *framework.Resource
		var limits *framework.Resource
		// pending pod is last in sequence
		if p == pod {
			*nodeRequestMinusPod = *nodeRequest
			*nodeLimitMinusPod = *nodeLimit
			requested = podRequests
			limits = podLimits
		} else {
			// get requests and limits for pod
			requested = GetResourceRequested(p)
			limits = GetResourceLimits(p)
			// make sure limits not less than requests
			SetMaxLimits(requested, limits)
		}

		// accumulate
		nodeRequest.MilliCPU += requested.MilliCPU
		nodeRequest.Memory += requested.Memory
		nodeLimit.MilliCPU += limits.MilliCPU
		nodeLimit.Memory += limits.Memory
	}
	// cap requests by node capacity
	setMin(&nodeRequest.MilliCPU, capCpu)
	setMin(&nodeRequest.Memory, capMem)
	setMin(&nodeRequestMinusPod.MilliCPU, capCpu)
	setMin(&nodeRequestMinusPod.Memory, capMem)

	klog.V(6).InfoS("Total node resources:", "node", klog.KObj(node),
		"CPU-req", nodeRequest.MilliCPU, "Memory-req", nodeRequest.Memory,
		"CPU-limit", nodeLimit.MilliCPU, "Memory-limit", nodeLimit.Memory,
		"CPU-cap", nodeCapacity.MilliCPU, "Memory-cap", nodeCapacity.Memory)

	return &NodeRequestsAndLimits{
		NodeRequest:         nodeRequest,
		NodeLimit:           nodeLimit,
		NodeRequestMinusPod: nodeRequestMinusPod,
		NodeLimitMinusPod:   nodeLimitMinusPod,
		Nodecapacity:        nodeCapacity,
	}
}

// SetMaxLimits : set limits to max(limits, requests)
// (Note: we could have used '(r *Resource) SetMaxResource(rl v1.ResourceList)', but takes map as arg )
func SetMaxLimits(requests *framework.Resource, limits *framework.Resource) {
	setMax(&limits.MilliCPU, requests.MilliCPU)
	setMax(&limits.Memory, requests.Memory)
	setMax(&limits.EphemeralStorage, requests.EphemeralStorage)
	if limits.AllowedPodNumber < requests.AllowedPodNumber {
		limits.AllowedPodNumber = requests.AllowedPodNumber
	}
	for k, v := range requests.ScalarResources {
		if limits.ScalarResources[k] < v {
			limits.ScalarResources[k] = v
		}
	}
}

// setMin : x <- min(x, y)
func setMin(x *int64, y int64) {
	if *x > y {
		*x = y
	}
}

// setMax : x <- max(x, y)
func setMax(x *int64, y int64) {
	if *x < y {
		*x = y
	}
}
