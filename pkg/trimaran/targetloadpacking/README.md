# Overview

This folder holds the `TargetLoadPacking` plugin implementation based on [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## TargetLoadPacking Plugin
`TargetLoadPacking` depends on [Load Watcher](https://github.com/paypal/load-watcher) service. Instructions to build and deploy load watcher can be found [here](https://github.com/paypal/load-watcher/blob/master/README.md).
`watcherAddress` argument below must be setup for `TargetLoadPacking` to work.

Apart from `watcherAddress`, you can configure the following in `TargetLoadPackingArgs`:

1) `targetUtilization` : CPU Utilization % target you would like to achieve in bin packing. It is recommended to keep this value 10 less than what you desire. Default if not specified is 40.
2) `defaultRequests` : This configures CPU requests for containers without requests or limits i.e. Best Effort QoS. Default is 1 core.
3) `defaultRequestsMultiplier` : This configures multiplier for containers without limits i.e. Burstable QoS. Default is 1.5

Following is an example config to achieve around 80% CPU utilization, with default CPU requests as 2 cores and requests multiplier as 2:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
- schedulerName: trimaran
  plugins:
    score:
      disabled:
      - name: NodeResourcesBalancedAllocation
      - name: NodeResourcesLeastAllocated
      enabled:
       - name: TargetLoadPacking
  pluginConfig:
  - name: TargetLoadPacking
    args:
      defaultRequests:
        cpu: "2000m"
      defaultRequestsMultiplier: "2"
      targetUtilization: 70 
      watcherAddress: http://127.0.0.1:2020
```