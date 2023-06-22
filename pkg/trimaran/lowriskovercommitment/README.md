# LowRiskOverCommitment Plugin

The `LowRiskOverCommitment` plugin is one of the `Trimaran` scheduler plugins, described in  [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling). The `Trimaran` plugins employ the `load-watcher` in order to collect measurements from the nodes as described [here](../README.md).

Though containers are allowed to specify resource limit values beyond the requested (guaranteed) values, most schedulers are only concerned with the requested values. This may result in a cluster where some (or all) nodes are overcommitted, thus leading to potential CPU congestion and performance degradation and/or memory OOM situations and pod evictions. The `LowRiskOverCommitment` plugin takes into consideration (1) the resource limit values of pods (limit-aware) and (2) the actual load (utilization) on the nodes (load-aware) could provide a low risk environment for pods and alleviate issues with overcommitment, while allowing pods to use their limits.

The `LowRiskOverCommitment` plugin evaluates the performance risk of overcommitment and selects the node with lowest risk. It achieves this goal by combining two risk factors: limit risk and load risk. The limit risk is based on requests and limits values. And, the load risk is based on observed load. `LowRiskOverCommitment` is risk-aware as well as load-aware. The outcome is that burstable and best effort pods are placed on nodes where the chance of being impacted by overcommitment is minimized, while providing them a chance to burst up to their full limits.

The `LowRiskOverCommitment` plugin has the following configuration parameters:

- `smoothingWindowSize` : The number of windows over which metrics are smoothed. (Default 5)
- `riskLimitWeights` : A map resource weights (between 0 and 1) of risk due to limit specifications (as opposed to risk due to load utilization). (Default [cpu: 0.5, memory: 0.5])

In addition, we have the `metricProvider`configuration parameters, depending on whether the `load-watcher` is in service or library mode, respectively.

Following is an example scheduler configuration with the `LowRiskOverCommitment` plugin enabled, and using the `load-watcher` in library mode, collecting measurements from the Prometheus server.

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
- schedulerName: trimaran
  plugins:
    score:
      enabled:
       - name: LowRiskOverCommitment
  pluginConfig:
  - name: LowRiskOverCommitment
    args:
      smoothingWindowSize: 5
      riskLimitWeights:
        cpu: 0.5
        memory: 0.5
      metricProvider:
        type: Prometheus
        address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
```
