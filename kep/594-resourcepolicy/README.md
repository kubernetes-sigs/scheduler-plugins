# Resource Policy

## Table of Contents

- Summary
- Motivation
   - Goals
   - Non-Goals
- Proposal
   - CRD API
   - Implementation details
- Use Cases
- Known limitations
- Test plans
- Graduation criteria
- Production Readiness Review Questionnaire
   - Feature enablement and rollback
- Implementation history

## Summary
This proposal introduces a plugin to allow users to specify the priority of different resources and max resource 
consumption for workload on differnet resources.

## Motivation
The machines in a Kubernetes cluster are typically heterogeneous, with varying CPU, memory, GPU, and pricing. To 
efficiently utilize the different resources available in the cluster, users can set priorities for machines of different 
types and configure resource allocations for different workloads. Additionally, they may choose to delete pods running 
on low priority nodes instead of high priority ones. 

### Use Cases

1. As a user of cloud services, there are some stable but expensive ECS instances and some unstable but cheaper Spot 
instances in my cluster. I hope that my workload can be deployed first on stable ECS instances, and during business peak 
periods, the Pods that are scaled out are deployed on Spot instances. At the end of the business peak, the Pods on Spot 
instances are prioritized to be scaled in.

### Goals

1. Develop a filter plugin to restrict the resource consumption on each unit for different workloads.
2. Develop a score plugin to favor nodes matched by a high priority unit.
3. Automatically setting deletion costs on Pods to control the scaling in sequence of workloads through a controller.

### Non-Goals

1. Modify the workload controller to support deletion costs. If the workload don't support deletion costs, scaling in 
sequence will be random.
2. When creating a ResourcePolicy, if the number of Pods has already violated the quantity constraint of the 
ResourcePolicy, we will not attempt to delete the excess Pods.


## Proposal

### CRD API
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
    ignorePreviousPod: false
  forceMaxNum: false
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

`Priority` define the priority of each unit. Pods will be scheduled on units with a higher priority. 
If all units have the same priority, resourcepolicy will only limit the max pod on these units.

`Strategy` indicate how we treat the nodes doesn't match any unit. 
If strategy is `required`, the pod can only be scheduled on nodes that match the units in resource policy. 
If strategy is `prefer`, the pod can be scheduled on all nodes, these nodes not match the units will be 
considered after all nodes match the units. So if the strategy is `required`, we will return `unschedulable` 
for those nodes not match the units.

`MatchLabelKeys` indicate how we group the pods matched by `podSelector` and `matchPolicy`, its behavior is like 
`MatchLabelKeys` in `PodTopologySpread`.

`matchPolicy` indicate if we should ignore some kind pods when calculate pods in certain unit.

If `forceMaxNum` is set `true`, we will not try the next units when one unit is not full, this property have no effect
when `max` is not set in units.

### Implementation Details

#### Scheduler Plugins

For each unit, we will record which pods were scheduled on it to prevent too many pods scheduled on it.

##### PreFilter
PreFilter check if the current pods match only one resource policy. If not, PreFilter will reject the pod.
If yes, PreFilter will get the number of pods on each unit to determine which units are available for the pod
and write this information into cycleState.

##### Filter
Filter check if the node belongs to an available unit. If the node doesn't belong to any unit, we will return
success if the strategy is `prefer`, otherwise we will return unschedulable.

Besides, filter will check if the pods that was scheduled on the unit has already violated the quantity constraint.
If the number of pods has reach the `maxCount`, all the nodes in unit will be marked unschedulable.

##### Score
If `priority` is set in resource policy, we will schedule pod based on `priority`. Default priority is 1, and minimum 
priority is 1.

Score calculation details: 

1. calculate priority score, `scorePriority = (priority-1) * 20`, to make sure we give nodes without priority a minimum 
score.
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


