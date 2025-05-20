# Sakkara: Hierarchical Group Scheduling

A scheduler plugin which scheduled a group of homogeneous pods on a cluster with a tree topology, satisfying multiple level placement constraints.

## Background

Named after a [step pyramid in Egypt](https://en.wikipedia.org/wiki/Saqqara), Sakkara places all pods in a group based on a rich set of topological constraints, given a physical hierarchical cluster topology. The resulting logical application topology is made available to the group for initial configuration that helps improve its running performance and cluster utilization. Topological constraints, such as pack, spread, partition, range, and factor policies, can be specified at any level of the topology, and each level can satisfy a different policy. This way, Sakkara can accommodate a flexible set of application-specific logical topologies. A unique feature is the support of priorities where jobs may be preempted as gangs. Sakkara employs a solver which uses the [chic-sched algorithm](https://github.com/ibm/chic-sched) to find a placement solution for the entire group, satisfying the group topological constraints.

## Using

Sakkara places **groups** of **pods**, satisfying specified topological constraints, on a **cluster hierarchical topology**.

### Cluster topology

The cluster topology is specified as a tree where the leaves are the nodes in the cluster.
There may be one or more levels in the tree, where nodes are at level 0.
For example, level 1 may represent servers, level 2 racks, level 3 rows, and so on.
If no cluster topology is specified, Sakkara assumes a flat topology with only one level, i.e. a tree with a root and all nodes in the cluster as leaves, with two resources: CPU and memory.

The tree is specified in a ConfigMap with a label `"sakkara.topology":`.
It may be updated at any time. Updates take effect during the following group placement cycle.

An example [cluster-topology.yaml](../../manifests/sakkara/cluster-topology.yaml) with a two-level tree of racks and nodes follows.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-topology
  namespace: sakkara-scheduler
  labels:
    "sakkara.topology": ""
data:
  name: "cluster-tree"
  resource-names: |
    [ "cpu", "memory", "nvidia.com/gpu" ]
  level-names: |
    [ "rack", "node" ]
  tree: |
    {
      "rack-0": {
        "node-0": {},
        "node-1": {},
        "node-2": {}
      },
      "rack-1": {
        "node-3": {},
        "node-4": {},
        "node-5": {}
      }
    }
```

- *namespace*: The topology configMap is deployed in a special namespace to avoid modifications by unauthorized users. The name of the namespace is defined using the argument `topologyConfigMapNameSpace` of the Sakkara scheduler plugin in the `KubeSchedulerConfiguration`. If not specified, it defaults to the `default` namespace. Here we use namespace sakkara-scheduler.
- *resource-names*: Resources to be considered when placing the group.
- *level-names*: Names of the levels as an array of strings ordered top to bottom (excluding the root).
- *tree*: String specification of the tree.

### Group constraints

Group definition and placement topological constraints are provided in a ConfigMap with label `"sakkara.group":`. An example [group-a.yaml](../../manifests/sakkara/group-a.yaml) of `group-a' consisting of 8 pods follows.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: group-a
  namespace: default
  labels:
    "sakkara.group": ""
data:
  "name": "group-a"
  "size": "8"
  "priority": "0"
  "constraints": |
    {
      "rack": {
        "type": "pack"
      },
      "node": {
        "type": "spread"
      }
    }
```

The constraints state that the 8 pods are to be placed in such a way that they are packed on racks, but spread on nodes within the rack. The constraints are soft by default. The resulting placement depends on the current allocation in the cluster. Ideally, the resulting placement would be:

```text
{root:
    {rack-0: 
        {node-0: {pod-0, pod-1, pod-2}},
        {node-1: {pod-3, pod-4, pod-5}},
        {node-2: {pod-6, pod-7}}
    }
}
```

In case rack-1 cannot accommodate the whole group, node-2 does not have enough availability for one pod, node-1 can accommodate up to 3 pods, and node-0 has plenty of space, we get:

```text
{root:
    {rack-0: 
        {node-0: {pod-0, pod-1, pod-2, pod-3, pod-4}},
        {node-1: {pod-5, pod-6, pod-7}}
    }
}
```

Other placements are also possible, where a maximum of 6 pods could be placed on rack-0 and the remaining 2 pods on rack-1:

```text
{root:
    {rack-0: 
        {node-0: {pod-0, pod-1, pod-2}},
        {node-2: {pod-3, pod-4, pod-5}}
    },
    {rack-1: 
        {node-3: {pod-6}},
        {node-5: {pod-7}}
    }
}
```

There are several types of constraints, which may be specified at the various levels of the hierarchy. If none is specified, the default is *pack*.

- *pack*: Pack as many pods as possible on a unit at the level.

```yaml
         "type": "pack"
```

- *spread*: Spread pods as evenly as possible on units at the level.

```yaml
         "type": "spread"
```

- *partition*: Divide the pods into a number of partitions of approximately equal size.

```yaml
         "type": "spread",
         "partitions": 3
```

- *range*: The number of pods placed on a unit at the level is within a range.
  
```yaml
         "min": 2,
         "max": 4
```

- *factor*: The number of pods placed on a unit at the level is a multiple of a given factor.
  
```yaml
         "factor": 4
```

Care has to be taken when specifying constraints, especially at multiple levels, as they may be inconsistent.

### Group pods

The group of pods are specified based on the kind of group. An example of a batch/v1 job group is provided [job-a.yaml](../../manifests/sakkara/job-a.yaml). In this case the pods of the group are specified in the pod template. For Sakkara, the following additions are needed.

- *group name*
  
```yaml
      labels:
        sakkara.group.name: "group-a"
```

- *scheduler name*

```yaml
    spec:
      schedulerName: sakkara-scheduler
```

The group may be made up of a collection of pods, or a deployment, or multiple deployments, e.g. master and worker pods. The main requirement is that pods are homogeneous, i.e. all pods in the group have the same amount of requested resources.

A group of pods without a corresponding group ConfigMap would remain in the pending state until the group ConfigMap is created.

### Placement results

In addition to the pods of a group being scheduled by Sakkara as a gang and according to topological constraints, some information about the placement result is provided in the group ConfigMap and the pods. This information may be used to configure the application containing the group.

- *group placement*

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: group-a
  namespace: default
  labels:
    "sakkara.group": ""
data:
  "name": "group-a"
  "size": "8"
  "priority": "0"
  "constraints": |
    {
      "rack": {
        "type": "pack"
      },
      "node": {
        "type": "spread"
      }
    }
  "status": Bound
  "placement": |
    {"root":
      {"rack-0": 
        {"node-0": {"pod-0", "pod-4", "pod-6"}},
        {"node-2": {"pod-1", "pod-3", "pod-7"}}
      },
      {"rack-1": 
        {"node-3": {"pod-2"}},
        {"node-3": {"pod-5"}}
      }
    }
  "rank": '[("pod-0",0) ("pod-4",1) ("pod-6",2) ("pod-1",3) ("pod-3",4) ("pod-7",5) ("pod-2",6) ("pod-5",7) ]'
```

Pods in the group are ranked based on their distance from a designated (master) pod. If a pod has the string "master" in its name, it will be considered the master pod, otherwise an arbitrary pod will be selected. Distance is the number of edges between two pods in the topology tree.

- *pod placement*

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-6
  labels:
    sakkara.group.name: group-a
    sakkara.member.rank: "2"
    sakkara.member.retries: "1"
    sakkara.member.status: Bound
```

The rank, status, and number of placement trials for the pod are provided as labels. This way, an (init) container in the pod may read the rank label and configure itself accordingly.

- *timing statistics*

Timing metrics about group scheduling is recorded in the group ConfigMap. This includes timestamps at the begin and end of group scheduling, the total scheduling time (including waiting in the pending state and retrials), and the amount of time taken by the chic-sched solver in Sakkara. An example follows.

```yaml
data:
  name: group-a
  schedBeginTime: "2024-07-02T10:53:58.62352-04:00"
  schedEndTime: "2024-07-02T10:53:58.777733-04:00"
  schedSolverTimeMilli: "0"
  schedTotalTimeMilli: "154"
```

### Optional: Using PodGroup

As an alternative to specifying group specifications in a ConfigMap, as described above, one could use the [PodGroup](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/pkg/coscheduling) resource. The PodGroup name would be the name of the group and the `minMember` specification would be the size of the group. The group priority corresponds to the priority of pods in the group.

When using a group ConfigMap, Sakkara provides more functionality, such as populating the ConfigMap with the group logical topology, timing metrics, and the ability start Sakkara cold after a crash, especially during preemption. Also, the ability to specify topological placement constraints at various levels. (**With PodGroups, the default is pack at all levels.**) Sakkara supports both forms of group specifications, however the ConfigMap takies precedence.

When using a PodGroup, the group name is provided in all pods in the group using label PodGroup label as follows.

```yaml
      labels:
        scheduling.x-k8s.io/pod-group: group-a
```

Similar to using the group ConFigMap, some placement results are reflected in the pods through labels as follows.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-6
  labels:
    scheduling.x-k8s.io/pod-group: group-a
    sakkara.member.rank: "2"
    sakkara.member.retries: "1"
    sakkara.member.status: Bound
```
