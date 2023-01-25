# LeastNUMANodes ScoringStrategy for NodeResourceTopologyMatch plugin

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [ScoringStrategy implementation details](#scoringstrategy-implementation-details)
    - [Description of calculating required NUMA nodes](#description-of-calculating-required-numa-nodes)
    - [Description of normalizing score](#description-of-normalizing-score)
    - [Example](#example)
- [Use cases](#use-cases)
- [Known limitations](#known-limitations)
- [Test plans](#test-plans)
- [Graduation criteria](#graduation-criteria)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
  - [Feature enablement and rollback](#feature-enablement-and-rollback)
- [Implementation history](#implementation-history)
<!-- /toc -->

# Summary

This document describes behaviour of a new ScoringStrategy `LeastNUMANodes` for NodeResourceTopologyMatch plugin that scores nodes based on how many NUMA nodes are required to run a given pod.

# Motivation

Consuming resources from multiple NUMA nodes can cause significant performance degradation
in latency-critical execution and high-throughput applications.
Topology Manager assigns resources from least amount of NUMA nodes but the scheduler is unaware
of different NUMA topologies. The best case scenario would be to schedule pod on the node that
can satisfy resource requirements from least amount of NUMA nodes to minimize latency.

## Goals

- Make better scheduling decisions that take NUMA topology into consideration.

## Non-Goals

- Change the PodSpec to allow requesting a maximum amount of NUMA nodes required to satisfy resource requirements.

# Proposal

A new ScoringStrategy `LeastNUMANodes` would check how many NUMA nodes are required to run a given pod. It will use [CRD][1]
to get available resources on the worker nodes and identify which topology policy is enabled, the same as other ScoringStrategies.

For now available ScoringStrategies are running only for `single-numa-node` TopologyManager policy.
`LeastNUMANodes` strategy can score nodes running all TopologyManager policies.

Kubernetes `v1.26` introduced `TopologyManagerPolicyOptions` that allows to control `TopologyManager` behaviour.
New option `prefer-closest-numa-nodes` will favor sets of NUMA nodes with shorter distance between them when making admission decisions.
`LeastNUMANodes` ScoringStrategy will also take distance between topology zones into consideration and favor nodes with the smallest average distance between zones.
Distances between topology zones are exposed as `Costs` field in [NRT CRD](https://github.com/k8stopologyawareschedwg/noderesourcetopology-api/blob/master/pkg/apis/topology/v1alpha1/types.go#L56).
This behaviour will be enabled by default, since the [CRD][1] doesn't expose `TopologyManagerPolicyOptions`,
those are planned to be included in the next version of [NRT CRD](https://github.com/k8stopologyawareschedwg/noderesourcetopology-api/pull/25).
When `TopologyManagerPolicyOptions` will be accessible in scheduler average distance between zones will be calculated only for worker nodes where `prefer-closest-numa-nodes` option is enabled.

## ScoringStrategy implementation details

Since topology of the node is stored in the CR, kube-scheduler should be subscribed for updates of appropriate CRD type.
Same mechanism is used by other ScoringStrategies for `NodeResourceTopologyMatch` plugin.

### Description of calculating required NUMA nodes

It's important to notice the we need to mimic how Topology Manger chooses which bitmask is the most suitable if there are multiple combinations of NUMA nodes that can satisfy the request.
The narrower bitmask is preffered or if the same number of bits is set then the mask with more lower-numbered bits set wins out([link](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/cm/topologymanager/bitmask/bitmask.go#L143)).

To calculate how many NUMA nodes are required to run given container/pod the plugin will:
* generate possible combinations starting from narrowest possible bitmask to widest
* find minimum average distance for combinations of given length
* the [Combination][2] function can used to generate combinations since it generate combinations from the lowest values
* combine resources available in combination by iterating over every Resources present in every NUMA node and adding them together
* each NUMA nodes combination will be evaluated to see if there are enough resources to satisfy POD requirements
* the average distances between nodes will be calculated for every combination, if it equals minimum average distance, return the combination
  if the minimum average cannot be find, look for combination with lowest average within given mask length
* it's valid to request only non NUMA resources, so if pod asks only for non NUMA resources, it requires 0 NUMA nodes
* return lowest number of required nodes and a boolean indicating if we can provide nodes mask with minimum average distance


The Topology Manager can deal with the alignment of resources in a couple of distinct scopes:
* container
* pod

Calculating required NUMA nodes for a `pod` scope is straightforward, we just need to calculate it for pod's effective resources.

When it comes to `container` scope we need to calculate `required` NUMA nodes for every container in proper order([same as TopologyManager asks for hints](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/cm/topologymanager/scope_container.go#L52))
and temporarly substract those resources from available resources in given combination and store only the maximum for given pod:
```go
    res, isMinDistance := getRequiredNUMANodes(container.Resources.Requests)
    // take max NUMA we want to score nodes based on the worst scenario
	if res > maxNuma {
		maxNuma = res
	}
```

For `container` scope all containers in a pod must be able to satisfy resource requiremnts from a set of nodes with lowest average distance to consider this allocation as optimal
from distance perspective.

### Description of normalizing score

Score will be calculated as follows:

```go
func normalizeScore(numaNodes int, isMinDistance bool) int64 {
	numaScore := framework.MaxNodeScore / highestNUMAID
	minDistanceModifier := 0
	if isMinDistance{
		minDistanceModifier = 1
	}
	return framework.MaxNodeScore - int64(numaNodes)*numaScore + minDistanceModifier * (numaScore / 2)
}
```

So pod which only uses non NUMA resources will receive `MaxNodeScore` and pod that requires 2 NUMA nodes but withouth providing
`minDistance` will receive:

```
100 - (2*(100/8)) + 0 * ((100/8)/2) = 100 - (2*(12)) = 100 - 24 = 76
```

A pod that requires 2 NUMA nodes providing `minDistance` will receive:

```
100 - (2*(100/8)) + 1 * ((100/8)/2) = 100 - (2*(12)) + (12/2) = 100 - 24 + 6 = 82
```

### Example

Suppose we have 2 candidate nodes after going through Filter phase. Each nodes has 2 NUMA nodes but available resources looks as follow:

**Node 1**

| Resource name | Available resource in NUMA node 0 | Available resource in node NUMA node 1 |
|---------------|-----------------------------------|----------------------------------------|
| cpu           | 2                                 | 4                                      |

Distance table

|       | node0 | node1 |
|-------|-------|-------|
| node0 | 10    | 20    |
|-------|-------|-------|
| node1 | 20    | 10    |

**Node 2**

| Resource name | Available resource in NUMA node 0 | Available resource in NUMA node 1 |
|---------------|-----------------------------------|-----------------------------------|
| cpu           | 8                                 | 8                                 |

|       | node0 | node1 |
|-------|-------|-------|
| node0 | 10    | 20    |
|-------|-------|-------|
| node1 | 20    | 10    |

The pod consists of 2 containers each asking for 3 cpus.

Score calculation for `Node 1` will look as follows:
- First container:
    - Generate combinations for 2 NUMA nodes starting from narrowest and lowest to widest and greatest
    - Generate combinations for bitmask of length 1 `[(10) (01)]`
    - Find minDistance for combinations -> (10) = 10 , (01) = 10, min is 10
    - Check if container can fit first combination (10) -> it can't
    - Check if container can fit second combination (01) -> it can, return  bitmask (01) and true(01 bitmask provides minDistance)
    - Substract container resources from combined resources:

    **Node 1**

    | Resource name | Available resource in NUMA node 0 | Available resource in NUMA node 1 |
    |---------------|-----------------------------------|-----------------------------------|
    | cpu           | 2                                 | 1                               |

- Second container:
    - Generate combinations for 2 NUMA nodes starting from narrowest and lowest to widest and greatest
    - Generate combinations for bitmask of length 1 `[(10) (01)]`
    - Find minDistance for combinations -> (10) = 10 , (01) = 10, min is 10
    - Check if container can fit first combination (10) -> it can't
    - Check if container can fit second combination (01) -> it can't
    - No bitmask with width of 1 can satisfy resource requirements of container -> increase bitmask width
    - Generate combinations for bitmask of length 2 `[(11)]`
    - Find minDistance for combinations -> (11) = 10 + 20 / 2 = 15
    - Combine resources from `node 0` and `node 1` -> available cpus 3
    - Check if container can fit third combination (11) -> it can, return bitmask (11) and true(11 bitmask provides minDistance)
    - Substract container resources from combined resources:

    **Node 1**

    | Resource name | Available resource in NUMA node 0 | Available resource in NUMA node 1 |
    |---------------|-----------------------------------|-----------------------------------|
    | cpu           | 0                                 | 0                                 |

- Score calculations:
  - Take max of required NUMA nodes `max([1, 2]) = 2`
  - Calculate normalize score = 76 + 6 = 82

Score calculation for `Node 2` will look as follows:
- First container:
    - Generate combinations for 2 NUMA nodes starting from narrowest and lowest to widest and greatest
    - Generate combinations for bitmask of length 1 `[(10) (01)]`
    - Check if container can fit second combination (10) -> it can, return  bitmask (10) and true(10 bitmask provides minDistance)
    - Substract container resources from combined resources:

    **Node 2**

    | Resource name | Available resource in NUMA node 0 | Available resource in NUMA node 1 |
    |---------------|-----------------------------------|-----------------------------------|
    | cpu           | 5                                 | 8                                 |

- Second container:
    - Generate combinations for 2 NUMA nodes starting from narrowest and lowest to widest and greatest
    - Generate combinations for bitmask of length 1 `[(10) (01)]`
    - Check if container can fit second combination (10) -> it can, return  bitmask (10) and true(10 bitmask provides minDistance)
    - Substract container resources from combined resources:

    **Node 2**

    | Resource name | Available resource in NUMA node 0 | Available resource in NUMA node 1 |
    |---------------|-----------------------------------|-----------------------------------|
    | cpu           | 2                                 | 8                                 |

- Score calculations:
  - Take max of required NUMA nodes `max([1, 1]) = 1`
  - Calculate normalize score = 88 + 6 = 94

# Use cases

Numbers of kubernetes worker nodes with different hardware configuration on bare metal with NUMA topology. TopologyManager feature gate enabled on the nodes. In this configuration, the user would like to deploy application and make sure it will land on a node where its resource requirements can be satisfied from the least amount of NUMA nodes to
minimize latency.

# Known limitations

Kube-scheduler makes an assumption about current resource usage on the worker node, since kube-scheduler knows which pod assigned to node. This assumption makes right after kube-scheduler choose a node. But in case of scheduling with NUMA topology only TopologyManager on the worker node knows exact NUMA node used by pod, this information about NUMA node delivers to kube-scheduler with latency. In this case kube-scheduler will not know actual NUMA topology until topology exporter will send it back. It could be mitigated if kube-scheduler in proposed plugin will add a hint on which NUMA id pod could be assigned, further Topology Manager on the worker node may take it into account.

# Test plans

It would be ensured that the components developed or modified for this feature can be easily tested.

* Unit Tests

Unit test for score scheduler plugin (pkg/numanodes/plugin.go)
pkg/numanodes/plugin_test.go which test the plugin.

* Integration Tests
    *  Default configuration (this plugin is disabled)
        * no side effect on basic scheduling flow (and performance)
        * no side effect no matter the CRD is installed or not
    *  Enable this plugin
        * basic workflow of this feature works (the pod lands on the node that requires the least amount of NUMA nodes to run it)
        * verify the behavior when the CRD is and isn't installed
* End-to-end tests

Integration and End-to-end would Implementation of it does not constitute a difficulty, but requires appropriate multi-numa hardware for comprehensive testing of this feature. Comprehensive E2E testing of this would be done in order to graduate this feature from Alpha to Beta.

# Graduation criteria

* Alpha (v0.25)

Following changes are required:
- [ ] Implementation of new ScoringStrategy
- [ ] Unit tests and integration tests from [Test plans](#test-plans).

* Beta
- [ ] Add node E2E tests.
- [ ] Provide beta-level documentation.

# Production Readiness Review Questionnaire

## Feature enablement and rollback
* **How can this feature be enabled / disabled in a live cluster?**
    - This plugin doesn't require special feature gate, but it expects: TopologyManager feature gate enabled on the worker node.

# Implementation history

- 2022-12-08: KEP created
- 2023-01-25: KEP updated to consider distance between topology zones when scoring nodes

[1]: https://github.com/k8stopologyawareschedwg/noderesourcetopology-api
[2]: https://pkg.go.dev/gonum.org/v1/gonum/stat/combin#Combinations
