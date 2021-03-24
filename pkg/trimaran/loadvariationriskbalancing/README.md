# LoadVariationRiskBalancing Plugin

The `LoadVariationRiskBalancing` plugin is one of the `Trimaran` scheduler plugins and is described in detail in  [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling). The `Trimaran` plugins employ the `load-watcher` in order to collect measurements from the nodes as described [here](../README.md).

The (normalized) risk of a node is defined as a combined measure of the average and standard deviation of the node utilization. It is given by

```latex
risk = [ average + margin * stDev^{1/sensitivity} ] / 2
```

where *average*​ and *stDev*​ are the fractional (between 0 and 1) measured average utilization and standard deviation of the utilization over a period of time, respectively. The two parameters: *margin*​ and *sensitivity*​, impact the amount of risk due to load variation. In order to magnify the impact of low variations, the *stDev*​ quantity is raised to a fractional power with the *sensitivity*​ parameter being the root power. And, the *margin*​ parameter scales the variation quantity. The recommended values for the *margin*​ and *sensitivity*​ parameters are 1 and 2, respectively. Each of the two added terms is bounded between 0 and 1. Then, the divisor 2 is used to normalize risk between 0 and 1.

(Since the additional load due to the pod, that is the subject of scheduling, is not known in advance, we assume that its average and standard deviation load are the requested amount and zero, respectively.)  

Risk is calculated independently for the CPU and memory resources on the node. Let *worstRisk* be the maximum of the two calculated risks. The *score* of the node, assuming that *minScore* is 0, is then computed as

```latex
score = maxScore * (1 - worstRisk)
```

Thus, the `LoadVariationRiskBalancing` plugin has the following configuration parameters:

- `safeVarianceMargin` : Multiplier (non-negative floating point) of standard deviation. (Default 1)
- `safeVarianceSensitivity` : Root power (non-negative floating point) of standard deviation. (Default 1)

In addition, we have the  `watcherAddress` or `metricProvider`configuration parameters, depending on whether the `load-watcher` is in service or library mode, respectively.

Following is an example scheduler configuration with the `LoadVariationRiskBalancing` plugin enabled, and using the `load-watcher` in library mode, collecting measurements from the Prometheus server.

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
      safeVarianceMargin: 1
      safeVarianceSensitivity: 2
      metricProvider:
        type: Prometheus
        address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
```
