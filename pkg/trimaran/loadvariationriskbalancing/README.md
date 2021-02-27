# Overview

This folder holds the `LoadVariationRiskBalancing` plugin implementation based on [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## LoadVariationRiskBalancing Plugin

`LoadVariationRiskBalancing` depends on [Load Watcher](https://github.com/paypal/load-watcher). 
It uses `load-watcher` in two modes.
1. Using `load-watcher` service as a metric endpoint.
   You can run `load-watcher` service separately to provide real time node resource usage metrics for `LoadVariationRiskBalancing` to consume.
   Instructions to build and deploy load watcher can be found [here](https://github.com/paypal/load-watcher/blob/master/README.md).
   In this way, you just need to configure `watcherAddress: http://xxxx.svc.cluster.local` to your `load-watcher` service. You can 
   also deploy `load-watcher` as a service in the same scheduler pod, 
   following the tutorial [here](https://medium.com/paypal-engineering/real-load-aware-scheduling-in-kubernetes-with-trimaran-a8efe14d51e2).

2. Using `load-watcher` as a library to fetch metrics from other providers, such as Prometheus, SignalFx and Kubernetes metric server.
   In this mode, you need to configure three parameters: `metricProviderType`, `metricProviderAddress` and `metricProviderToken` if authentication is needed.

   By default, `metricProviderType` is `KubernetesMetricsServer` if not set. Now it supports `KubernetesMetricsServer`, `Prometheus` and `SignalFx`.
    - `metricProviderType: KubernetesMetricsServer` use `load-watcher` as a client library to retrieve metrics from Kubernetes metric
      server directly.
    - `metricProviderType: Prometheus` use `load-watcher` as a client library to retrieve metrics from Prometheus directly.
    - `metricProviderType: SignalFx` use `load-watcher` as a client library to retrieve metrics from SignalFx directly.

   `metricProviderAddress` and `metricProviderToken` should be configured according to `metricProviderType`.
    - You can ignore `metricProviderAddress` when using `metricProviderType: KubernetesMetricsServer`
    - Configure the prometheus endpoint for `metricProviderAddress` when using `metricProviderType: Prometheus`.
      An example could be `http://prometheus-k8s.monitoring.svc.cluster.local:9090`.
      
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
      metricProviderType: Prometheus
      metricProviderAddress: http://prometheus-k8s.monitoring.svc.cluster.local:9090
```
