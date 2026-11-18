# Overview

This folder holds the NetworkAware plugins implemented as discussed in the [KEP - Network-Aware framework](https://github.com/kubernetes-sigs/scheduler-plugins/pull/282).

## Maturity Level

- [x] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## TopologicalSort Plugin (QueueSort)

The `TopologicalSort` **QueueSort** plugin orders pods to be scheduled in an [**AppGroup**](https://github.com/diktyo-io/appgroup-api) based on their 
microservice dependencies related to [TopologicalSort](https://en.wikipedia.org/wiki/Topological_sorting).
 
Further details and examples are described [here](../networkaware/topologicalsort).

## NetworkOverhead Plugin (Filter & Score)

The `NetworkOverhead` **Filter & Score** plugin filters out nodes based on microservice dependencies 
defined in an **AppGroup** and scores nodes with lower network costs (described in a [**NetworkTopology**](https://github.com/diktyo-io/networktopology-api))
higher to achieve latency-aware scheduling.

Further details and examples are described [here](../networkaware/networkoverhead). 

## Scheduler Config example 

Consider the following scheduler config as an example to enable both plugins:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "/etc/kubernetes/scheduler.conf"
profiles:
  - schedulerName: network-aware-scheduler
    plugins:
      multiPoint:
        enabled:
          - name: NetworkOverhead
            weight: 5 # A higher weight is given to NetworkOverhead to favor allocation schemes with lower latency.
          - name: BalancedAllocation
            weight: 1
        disabled:
          - name: NodeResourcesFit # Preferably avoid the combination of NodeResourcesFit with NetworkOverhead
      queueSort:
        enabled:
          - name: TopologicalSort
        disabled:
          - name: "*"
    pluginConfig:
      - name: TopologicalSort
        args:
          namespaces:
            - "default"
      - name: NetworkOverhead
        args:
          namespaces:
            - "default"
          weightsName: "UserDefined" # The respective weights to consider in the plugins
          networkTopologyName: "net-topology-test" # networkTopology CR to be used by the plugins
```

## Spreading replicas across zones

`NetworkOverhead` scores nodes by the network cost to a pod's dependencies, so replicas of the same workload
can end up in the same zone, where one zone failure takes out all of them. At the weight in the example above,
the `NetworkOverhead` score can outweigh the default `PodTopologySpread` and `InterPodAffinity` scores (weight 2
each), so `whenUnsatisfiable: ScheduleAnyway` constraints and preferred pod anti-affinity may not spread the
replicas. If a workload has to survive a zone failure, add a zone constraint with `DoNotSchedule`, which is
enforced at Filter, to the pod template's `spec`:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app: my-app # the workload's own pod label
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app: my-app
```

Constraints set on a pod replace the scheduler's default spreading, so keep the hostname constraint too. With
`DoNotSchedule`, a pod stays Pending when every zone it could go to without exceeding `maxSkew` has no node that
passes the other filters, including the `NetworkOverhead` Filter.

## Summary

Further details about the network-aware framework are available [here](../../kep/260-network-aware-scheduling/README.md).
