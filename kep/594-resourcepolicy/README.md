# Resource Policy

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Use Cases](#use-cases)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [CRD API](#crd-api)
  - [Implementation Details](#implementation-details)
    - [Scheduler Plugins](#scheduler-plugins)
      - [PreFilter](#prefilter)
      - [Filter](#filter)
      - [Score](#score)
    - [Resource Policy Controller](#resource-policy-controller)
- [Known limitations](#known-limitations)
- [Test plans](#test-plans)
- [Graduation criteria](#graduation-criteria)
- [Feature enablement and rollback](#feature-enablement-and-rollback)
<!-- /toc -->

## Summary
This proposal introduces a plugin that enables users to set priorities for various resources and define maximum resource consumption limits for workloads across different resources.

## Motivation
A Kubernetes cluster typically consists of heterogeneous machines, with varying SKUs on CPU, memory, GPU, and pricing. To
efficiently utilize the different resources available in the cluster, users can set priorities for machines of different 
types and configure resource allocations for different workloads. Additionally, they may choose to delete pods running 
on low priority nodes instead of high priority ones. 

### Use Cases

1. As a administrator of kubernetes cluster, there are some static but expensive VM instances and some dynamic but cheaper Spot 
instances in my cluster. I hope to restrict the resource consumption on each kind of resource for different workloads to limit the cost. 
I hope that important workloads in my cluster can be deployed first on static VM instances so that they will not worry about been preempted. And during business peak periods, the Pods that are scaled up are deployed on cheap, spot instances. At the end of the business peak, the Pods on Spot 
instances are prioritized to be scaled down.

### Goals

1. Develop a filter plugin to restrict the resource consumption on each kind of resource for different workloads.
2. Develop a score plugin to favor nodes matched by a high priority kind of resource.
3. Automatically setting deletion costs on Pods to control the scaling in sequence of workloads through a controller.

### Non-Goals

1. Scheduler will not delete the pods.

## Proposal

### API
```yaml
apiVersion: scheduling.sigs.x-k8s.io/v1alpha1
kind: ResourcePolicy
metadata:
  name: xxx
  namespace: xxx
spec:
  matchLabelKeys:
    - pod-template-hash
  matchPolicy:
    ignoreTerminatingPod: true
  podSelector:
    matchExpressions:
      - key: key1
        operator: In
        values:
        - value1
    matchLabels:
      key1: value1
  strategy: prefer
  units:
  - name: unit1
    priority: 5
    maxCount: 10
    nodeSelector:
      matchExpressions:
      - key: key1
        operator: In
        values:
        - value1
  - name: unit2
    priority: 5
    maxCount: 10
    nodeSelector:
      matchExpressions:
      - key: key1
        operator: In
        values:
        - value2
  - name: unit3
    priority: 4
    maxCount: 20
    nodeSelector:
      matchLabels:
        key1: value3
```

```go
type ResourcePolicy struct {
  ObjectMeta
  TypeMeta

  Spec ResourcePolicySpec
}
type ResourcePolicySpec struct {
  MatchLabelKeys []string
  MatchPolicy MatchPolicy
  Strategy string
  PodSelector metav1.LabelSelector
  Units []Unit
}
type MatchPolicy struct {
  IgnoreTerminatingPod bool
}
type Unit struct {
  Priority *int32
  MaxCount *int32
  NodeSelector metav1.LabelSelector
}
```

Pods will be matched by the ResourcePolicy in same namespace when the `.spec.podSelector`. And if `.spec.matchPolicy.ignoreTerminatingPod` is `true`, pods with Non-Zero `.spec.deletionTimestamp` will be ignored.
ResourcePolicies will never match pods in different namesapces. One pod can not be matched by more than one Resource Policies.

Pods can only be scheduled on units defined in `.spec.units` and this behavior can be changed by `.spec.strategy`. Each item in `.spec.units` contains a set of nodes that match the `NodeSelector` which describes a kind of resource in the cluster.

`.spec.units[].priority` define the priority of each unit. Units with higher priority will get higher score in the score plugin.
If all units have the same priority, resourcepolicy will only limit the max pod on these units.
If the `.spec.units[].priority` is not set, the default value is 0.
`.spec.units[].maxCount` define the maximum number of pods that can be scheduled on each unit. If `.spec.units[].maxCount` is not set, pods can always be scheduled on the units except there is no enough resource.

`.spec.strategy` indicate how we treat the nodes doesn't match any unit. 
If strategy is `required`, the pod can only be scheduled on nodes that match the units in resource policy. 
If strategy is `prefer`, the pod can be scheduled on all nodes, these nodes not match the units will be 
considered after all nodes match the units. So if the strategy is `required`, we will return `unschedulable` 
for those nodes not match the units.

`.spec.matchLabelKeys` indicate how we group the pods matched by `podSelector` and `matchPolicy`, its behavior is like 
`.spec.matchLabelKeys` in `PodTopologySpread`.

### Implementation Details

#### PreFilter
PreFilter check if the current pods match only one resource policy. If not, PreFilter will reject the pod.
If yes, PreFilter will get the number of pods on each unit to determine which units are available for the pod
and write this information into cycleState.

#### Filter
Filter check if the node belongs to an available unit. If the node doesn't belong to any unit, we will return
success if the `.spec.strategy` is `prefer`, otherwise we will return unschedulable.

Besides, filter will check if the pods that was scheduled on the unit has already violated the quantity constraint.
If the number of pods has reach the `.spec.unit[].maxCount`, all the nodes in unit will be marked unschedulable.

#### Score
If `.spec.unit[].priority` is set in resource policy, we will schedule pod based on `.spec.unit[].priority`. Default priority is 0, and minimum 
priority is 0.

Score calculation details: 

1. calculate priority score, `scorePriority = (priority) * 20`, to make sure we give nodes without priority a minimum score.
2. normalize score

#### Resource Policy Controller
Resource policy controller set deletion cost on pods when the related resource policies were updated or added.

## Known limitations

- Currently deletion costs only take effect on deployment workload.

## Test plans

1. Add detailed unit and integration tests for the plugin and controller.
2. Add basic e2e tests, to ensure all components are working together.
   
## Graduation criteria

This plugin will not be enabled only when users enable it in scheduler framework and create a resourcepolicy for pods.
So it is safe to be beta.

* Beta
- [ ] Add node E2E tests.
- [ ] Provide beta-level documentation.

## Feature enablement and rollback

Enable resourcepolicy in MultiPointPlugin to enable this plugin, like this:

```yaml
piVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
- schedulerName: default-scheduler
  plugins:
    multiPoint:
      enabled:
      - name: resourcepolicy
```


