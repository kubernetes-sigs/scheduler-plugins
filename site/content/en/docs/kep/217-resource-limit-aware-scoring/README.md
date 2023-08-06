---
title: Resource limit-aware node scoring
---

# Resource limit-aware node scoring

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Use Cases](#use-cases)
- [Proposal](#proposal)
- [Design Details](#design-details)
- [Implementation](#implementation)
- [Known Limitations](#known-limitations)
- [Alternatives considered](#alternatives-considered)
- [Graduation Criteria](#graduation-criteria)
- [Testing Plan](#testing-plan)
- [Implementation History](#implementation-history)
- [References](#references)
<!-- /toc -->

## Summary

This proposal adds a scoring plugin that scores a node based on the ratio of  pods' resource limit to the node's allocatable resource. The higher the ratio is, the less desirable the node is. By spreading resource limits across nodes, the plugin can reduce node resource over-subscription and resource contention on nodes running burstable pods.

## Motivation

Burstable pods in Kubernetes can have higher pod resource limits than resource requests. Because Kubernetes scheduler only takes into account resource requests during pod scheduling, resource limits on a node can be over-subscribed, i.e., the total resource limits of scheduled pods’ on a node exceeding the node’s allocatable capacity. Burstable pods typically have different limit to request ratios. As a result, resource limit to allocatable ratios on different nodes in a cluster (i.e., the total resource limit of running pods on a node to the node’s allocatable ratio) can vary a lot. We have noticed nodes' limit to allocatable ratio vary from 0.1 to 6 in production clusters. Over-subscription can cause resource contention, performance issues and even pod failure (e.g., OOM).

To reduce the extent of resource over-subscription and associated resource contention risks, we propose a scoring plugin that considers a node's resource limit in scheduling.

## Goals
Introduce a scoring plugin to mitigate resource over-subscription issue caused by burstable pods through "spreading" or "balancing" pod's resource limits across nodes.

## Non-Goals
Replace existing resource allocated plugin.

## Use Cases
Consider two nodes with the same resource allocatable (8 CPU cores) and each has two pods running.
- node1: pod1(request: 2, limit: 6), pod2(request: 2, limit: 4); total request/allocatable: 4/8, total limit/allocatable: 10/8
- node2: pod3(request: 3, limit: 3), pod4(request: 2, limit: 2); total request/allocatable: 5/8, total limit/allocatable: 5/8

**Default scheduler**

To schedule a new pod pod5 (request: 1, limit: 4), the default scheduler with `LeastAllocated` will choose node1 over node2 as node1 has a smaller request/allocatable ratio (4/8 vs. 5/8). The resulting node resource status will be
- node1: request/allocatable = 5/8, limit/allocatable = 14/8
- node2: request/allocatable = 5/8, limit/allocatable = 5/8

node1's resource limit is oversubscribed with a limit to allocatable ratio 14/8.  It will have a high risk of resource contention.

**Proposed scheduler**

If we consider node1's resource limit is already oversubscribed and hence place pod5 on node 2 instead, the result will be
- node1: total request/allocatable = 4/8, total limit/allocatable = 10/8
- node2: total request/allocatable = 6/8, total limit/allocatable = 9/8

node1’s total resource limit remains the same and node2 has a total resource limit of 9. node2 will have a lower risk of resource contention with pod5 running on it.

## Proposal

A custom scheduler scoring plugin scores a node by considering existing pods’ resource limits to the node's allocatable ratio on the node. It attempts to “evenly” spread or distribute pod resource limits across nodes in a cluster.

## Design Details

When choosing a node to place a pod, the custom scoring plugin prefers the node that has the smallest total resource limit ratio on a node. The scoring function favors a node with the smallest over-subscription ratio.

   `node limit ratio = sum of existing pods' limits on node / node’s allocatable resource`

1. Calculate each node’s raw score. A node's raw score will be negative for an over-subscription node.

    - `RawScore = (allocatable - limit) * MaxNodeScore / allocatable`, where limit is the sum of existing pods on the node and the new pod's resource limits.

    - For multiple resources, the raw score is the weighted sum of all allocatable resources. The resources and their weights are configurable as the plugin arguments. Like the other `NodeResources` scoring plugins, the plugin supports standard resource types: CPU, Memory, Ephemeral-Storage plus any Scalar-Resources.

2. Normalize the raw score to `[MinNodeScore , MaxNodeScore]` (e.g., [0, 100]) after all nodes are scored.

    - `NormalizedScore = MinNodeScore+(RawScore-LowestRawScore)/(HighestRawScore-LowestRawScore) *(MaxNodScore - MinNodeScore)`

3. Choose the node with the highest score.

## Implementation

- A node's allocatable can be directly obtained from `NodeInfo.Allocatable`.

- Calcuate the scheduling pods' limit
```go
func calculatePodResourceLimit(pod *v1.Pod, resource v1.ResourceName) int64 {
    var podLimit int64
    for _, container := range pod.Spec.Containers {
        value := getLimitForResource(resource, &container.Resources.Limits)
        podLimit += value
    }

    // podResourceLimit = max(sum(podSpec.Containers), podSpec.InitContainers) + overHead
    for _, initContainer := range pod.Spec.InitContainers {
        value := getLimitForResource(resource, &initContainer.Resources.Limits)
        if podLimit < value {
            podLimit = value
        }
    }

    // If Overhead is being utilized, add it to the total limit for the pod
    if pod.Spec.Overhead != nil && utilfeature.DefaultFeatureGate.Enabled(features.PodOverhead) {
        if quantity, found := pod.Spec.Overhead[resource]; found {
            podLimit += quantity.Value()
        }
    }

    return podLimit
}
```

- A node's resource limit of all running pods on the node is calculated from `NodeInfo.Pods`.

```go
// calculateAllocatedLimit returns the allocated resource limits on a node
 func calculateAllocatedLimit(node *framework.NodeInfo, resource v1.ResourceName) int64 {
    var limit int64
    for _, pod := range node.Pods {
        for _, container := range pod.Pod.Spec.Containers {
            limit += getLimitForResource(resource, &container.Resources.Limits)
        }
    }
    return limit
 }

  // GetLimitForResource returns a specified resource limit for a container
 func getLimitForResource(resource v1.ResourceName, limits *v1.ResourceList) int64 {
    switch resource {
    case v1.ResourceCPU:
        return limits.Cpu().MilliValue()
    case v1.ResourceMemory:
        return limits.Memory().Value()
        case v1.ResourceEphemeralStorage:
            return limits.EphemeralStorage().Value()
    default:
        if v1helper.IsScalarResourceName(resource) {
            quantity, found := (*limits)[resource]
            if !found {
                return 0
            }
            return quantity.Value()
        }
    }
    return 0
 }
```

- For a best effort pod without specifying a resource limit, a default limit value (e.g., the node's allocatable or configurable value) is used.
- Guaranteed pods will be included and calcuated like burstable pods. It means that the proposed plugin will enhance the effect of the default `LeastAllocated` plugin.

- Multiple resource types

  - The plugin will supports `CPU`, `Memory`, `EmphemeralStorage` and any `ScalarResources`, including `HugePage`.
  - The default resources will be CPU and Memory with the same weight of 1.
```
	{Name: string(v1.ResourceCPU), Weight: 1},
    {Name: string(v1.ResourceMemory), Weight: 1}
```

## Known Limitations

In the first version (alpha), the total limits of a node's pods will be calculated on demand, i.e., on the Score() phase of each scheduling cycle, on each node. Given the scoring plugins will run in parallel, the complexity will be `O(# of pods per node)`.

Once we decide to bump the maturity level to beta, or merge it into the default scheduler as an in-tree plugin, we need to optimize the performance such as caching the node's limits upon receiving pod events (add/update/delete).


## Alternatives considered

## Graduation Criteria

## Testing Plan
Unit tests and integration tests will be added.

## Implementation History
Implementation will be added.

## References
