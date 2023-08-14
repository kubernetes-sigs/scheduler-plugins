# Overview

The PostFilter extension point was introduced in Kubernetes Scheduler since 1.19,
and the default implementation in upstream is to preempt Pods **on the same node**
to make room for the unschedulable Pod.

In contrast to the "same-node-preemption" strategy, we can come up with a "cross-node-preemption"
strategy to preempt Pods across multiple nodes, which is useful when a Pod cannot be
scheduled due to "cross node" constraints such as PodTopologySpread and PodAntiAffinity.
This was also mentioned in the original design document of [Preemption].

[Preemption]: https://github.com/kubernetes/community/blob/master/contributors/design-proposals/scheduling/pod-preemption.md#supporting-cross-node-preemption

This plugin is built as a sample to demonstrate how to use PostFilter extension point,
as well as inspiring users to built their own innovative strategies, such as preepmpting
a group of Pods.

> ⚠️ CAVEAT: Current implementation doesn't do any branch cutting, but uses a DFS algorithm
> to iterate all possible preemption strategies. DO NOT use it in your production env.

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [x] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Example config:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: default-scheduler
  plugins:
    postFilter:
      enabled:
      - name: CrossNodePreemption
```
