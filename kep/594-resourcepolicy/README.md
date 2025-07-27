# Resource Policy

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Use Cases](#use-cases)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [API](#api)
  - [Implementation Details](#implementation-details)
    - [PreFilter](#prefilter)
    - [Filter](#filter)
    - [Score](#score)
    - [PreBind](#prebind)
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
  podSelector:
    key1: value1
  strategy: prefer
  units:
  - name: unit1
    max: 10
    maxResource:
      cpu: 10
    nodeSelector:
      key1: value1
```

```go
type ResourcePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePolicySpec   `json:"spec"`
	Status ResourcePolicyStatus `json:"status,omitempty"`
}

type ResourcePolicySpec struct {
	// +optional
	// +nullable
	// +listType=atomic
	Units []Unit `json:"units,omitempty" protobuf:"bytes,1,rep,name=units"`

	Selector       map[string]string `json:"selector,omitempty" protobuf:"bytes,2,rep,name=selector"`
	MatchLabelKeys []string          `json:"matchLabelKeys,omitempty" protobuf:"bytes,3,rep,name=matchLabelKeys"`
}

type Unit struct {
	Max          *int32          `json:"max,omitempty" protobuf:"varint,1,opt,name=max"`
	MaxResources v1.ResourceList `json:"maxResources,omitempty" protobuf:"bytes,2,rep,name=maxResources"`

	NodeSelector map[string]string `json:"nodeSelector,omitempty" protobuf:"bytes,3,rep,name=nodeSelector"`

	PodLabelsToAdd      map[string]string `json:"podLabels,omitempty" protobuf:"bytes,4,rep,name=podLabels"`
	PodAnnotationsToAdd map[string]string `json:"podAnnotations,omitempty" protobuf:"bytes,5,rep,name=podAnnotations"`
}

type ResourcePolicyStatus struct {
	Pods           []int64      `json:"pods,omitempty"`
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}
```

Pods will be matched by the ResourcePolicy in same namespace when the `.spec.podSelector`.
ResourcePolicies will never match pods in different namesapces. One pod can not be matched by more than one Resource Policies.

Pods can only be scheduled on units defined in `.spec.units`. Each item in `.spec.units` contains a set of nodes that match the `NodeSelector` which describes a kind of resource in the cluster.

Pods will be scheduled in the order defined by the `.spec.units`.
`.spec.units[].max` define the maximum number of pods that can be scheduled on each unit. If `.spec.units[].max` is not set, pods can always be scheduled on the units except there is no enough resource.
`.spec.units[].maxResource` define the maximum resource that can be scheduled on each unit. If `.spec.units[].maxResource` is not set, pods can always be scheduled on the units except there is no enough resource.

`.spec.matchLabelKeys` indicate how we group the pods matched by `podSelector`, its behavior is like
`.spec.matchLabelKeys` in `PodTopologySpread`.

### Implementation Details

#### PreFilter
PreFilter check if the current pods match only one resource policy. If not, PreFilter will reject the pod.
If yes, PreFilter will get the number of pods on each unit to determine which units are available for the pod
and write this information into cycleState.

#### Filter
Filter check if the node belongs to an available unit. If the node doesn't belong to any unit, we will return unschedulable.

Besides, filter will check if the pods that was scheduled on the unit has already violated the quantity constraint.
If the number of pods has reach the `.spec.unit[].max`, all the nodes in unit will be marked unschedulable.

#### Score

Node score is `100 - (index of the unit)`

#### PreBind

Add annotations and labels to pods to ensure they can be scaled down in the order of the units.

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


