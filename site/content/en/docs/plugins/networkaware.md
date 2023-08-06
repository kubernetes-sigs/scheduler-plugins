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

Further details and examples are described [here](./topologicalsort.md).

## NetworkOverhead Plugin (Filter & Score)

The `NetworkOverhead` **Filter & Score** plugin filters out nodes based on microservice dependencies
defined in an **AppGroup** and scores nodes with lower network costs (described in a [**NetworkTopology**](https://github.com/diktyo-io/networktopology-api))
higher to achieve latency-aware scheduling.

Further details and examples are described [here](./networkoverhead.md).

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
      queueSort:
        enabled:
          - name: TopologicalSort
        disabled:
          - name: "*"
      preFilter:
        enabled:
          - name: NetworkOverhead
      filter:
        enabled:
          - name: NetworkOverhead
      score:
        disabled: # Preferably avoid the combination of NodeResourcesFit with NetworkOverhead
          - name: NodeResourcesFit
        enabled: # A higher weight is given to NetworkOverhead to favor allocation schemes with lower latency.
          - name: NetworkOverhead
            weight: 5
          - name: BalancedAllocation
            weight: 1
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

## Summary

Further details about the network-aware framework are available [here](../kep/260-network-aware-scheduling/README.md).
