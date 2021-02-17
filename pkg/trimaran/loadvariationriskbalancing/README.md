# Overview

This folder holds the `LoadVariationRiskBalancing` plugin implementation based on [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## LoadVariationRiskBalancing Plugin

`LoadVariationRiskBalancing` depends on [Load Watcher](https://github.com/paypal/load-watcher) service. Instructions to build and deploy load watcher can be found [here](https://github.com/paypal/load-watcher/blob/master/README.md).
`watcherAddress` argument below must be setup for `LoadVariationRiskBalancing` to work.

Apart from `watcherAddress`, you can configure the following in `LoadVariationRiskBalancingArgs`:

1) `safeVarianceMargin` : Multiplier of standard deviation. Default is 1.

Following is an example config:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
- schedulerName: trimaran
  plugins:
    score:
      enabled:
       - name: LoadVariationRiskBalancing
  pluginConfig:
  - name: LoadVariationRiskBalancing
    args:
      safeVarianceMargin: "1"
      watcherAddress: http://127.0.0.1:2020
```
