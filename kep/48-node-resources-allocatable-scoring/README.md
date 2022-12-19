# Node resources allocatable scoring

## Table of Contents

<!-- toc -->
- [Motivation](#motivation)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Use Cases](#use-cases)
- [Terms](#terms)
- [Proposal](#proposal)
- [Design Details](#design-details)
  - [Extension points](#extension-points)
    - [Score](#score)
- [Known Limitations](#known-limitations)
- [Alternatives considered](#alternatives-considered)
- [Graduation Criteria](#graduation-criteria)
- [Testing Plan](#testing-plan)
- [Implementation History](#implementation-history)
- [References](#references)
<!-- /toc -->

## Motivation
A Kubernetes cluster can consist of nodes with widely varying resource capacities. Likewise, workloads may consist of pods with widely varying resource requests. A pod with high resource requests might only fit on one of the largest capacity nodes. If one or more small pods are scheduled on the largest capacity nodes, the large pod may be unscheduleable. That is a sub-optimal scheduling when there are smaller nodes available that could have taken the small pods. The existing scheduling priorities don't offer a solution to this scenario. For instance, the in-tree Score plugins ```NodeResourcesLeastAllocated``` and ```NodeResourcesMostAllocated``` use the **ratio of allocated resources** when deciding where to schedule a pod. Unfortunately that strategy does not address the use case since the ratio of allocated resources may still favor placing a small pod on a large node. To satisfy the use case, the node scores must be independent of existing pods on the node. To improve cluster utilization, this proposal tries to solve the problem by implementing a scoring plugin ```NodeResourcesAllocatable``` based on the scheduler framework, which uses only the node's **absolute number of allocatable resources**.

## Goals
1. Use a scheduler plugin, which is the most Kubernetes native way, to implement node resources allocatable scoring.
2. Provide args to score nodes based on node resources least allocatable or most allocatable.
3. Provide args to specify different weights for each resource used in the scoring calculations.

## Non-Goals

## Use Cases
1. When small and/or large pods may be deployed on a cluster where the large pods can only fit on a subset of nodes (e.g. the largest capacity nodes), the small pods should be scheduled such that there is room, if possible, to schedule the large pods when they are deployed. Otherwise the small pods may land on the large nodes and block the large pods from getting scheduled. If the scheduler uses the ```NodeResourcesAllocatable``` score plugin configured to prioritize least allocatable, that satisfies this use case.

2. Consider a cluster where there are a few permanent large capacity nodes and other smaller capacity ephemeral nodes. The ephemeral nodes may be added to or removed from the cluster based on demand. If unutilized, an ephemeral node may be removed to reduce costs. If we are optimizing to reduce costs, then the scheduler should prioritize placing pods on the large capacity nodes. If the scheduler uses the ```NodeResourcesAllocatable``` score plugin configured to prioritize most allocatable, that satisfies this use case.

## Terms

## Proposal

In order to implement node resources allocatable scoring, we developed a score plugin. The plugin takes an arg ```mode```, which can have values ```Least``` or ```Most```. When ```Least``` is specified nodes with the least allocatable resources are scored higher. When ```Most``` is specified, nodes with the most allocatable resources are scored higher. ```Least``` and ```Most``` are totally opposite semantics, so it's only possible to have one enabled in a scheduler profile. The plugin takes another arg ```resources``` to specify the ```weight``` and ```name``` of each resource used in the scoring calculations.

## Design Details
We implement a score plugin based on the scheduler framework. Implementation follows a similar pattern to the existing ```NodeResourcesLeastAllocated``` score plugin. Each allocatable resource of a node is weighted and summed. After scores are calculated for all nodes, we normalize the scores. Here is a concrete example, comparing plugin ```NodeResourcesLeastAllocatable``` with ```NodeResourcesLeastAllocated```:

Consider the following scenario:
* 2 nodes with allocated/allocatable of 0/10, 0/200.
* 4 pods are scheduled in the following order with the following requests: 5, 5, 100, 100

With score plugin ```NodeResourcesLeastAllocatable```, all pods are scheduled.
The final nodes have allocated/allocatable of 10/10, 200/200.

With score plugin ```NodeResourceLeastAllocated```, one pod may be unscheduleable.
The first pod may be placed on either node as both have equal ratio of allocated resources. Note that any scoring strategy that relies on ratio of allocated, will have this same issue. Even supposing the first pod landed on the smaller node, the second pod will be placed on the larger node as it has the least ratio of allocated resources. The final nodes have allocated/allocatable of 5/10, 105/200.

Consider another scenario with churn:

* 2 nodes with allocated/allocatable of 10/20 ( 1 pod: size 10), and 90/100 (2 pods: size 50 and 40)
* 1 pod of size 10 comes, pod of size 40 is terminated, a new pod of size 50 is scheduled.

With the ```NodeResourcesLeastAllocatable``` score plugin, all pods are scheduled. With other plugins, the size 10 pod may have been scheduled on the larger node and blocked the subsequent size 50 pod.

### Extension points

#### Score
To score a node we take the sum of it's weighted allocatable resources. The resources and weights to use are user configurable via the plugin args. If the mode is ```Least```, then we use the negation of the weighted sum. After all nodes are scored, the scores are normalized to a fixed range defined by the min and max node score in the framework (e.g. [0-100]).

## Known Limitations

## Alternatives considered
A couple of alternatives were proposed in the issue (see References section below), but none of them satisfied the Goals.

## Graduation Criteria

## Testing Plan
1.  Add detailed unit and integration tests for workloads.
2.  Add basic e2e tests, to ensure all components are working together.

## Implementation History
## References
- [NodeResourcesLeastAllocatable as score plugin](https://github.com/kubernetes/kubernetes/issues/93547)
