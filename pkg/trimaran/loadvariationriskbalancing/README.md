# LoadVariationRiskBalancing Plugin

The `LoadVariationRiskBalancing` is one of the `trimaran` scheduler plugins and is described in detail in  [Trimaran: Real Load Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/61-Trimaran-real-load-aware-scheduling). The configuration of the `trimaran` plugins is described [here](../README.md).

Apart from `watcherAddress` configuration parameters, the `LoadVariationRiskBalancing` plugin has the following parameter.

- `safeVarianceMargin` : Multiplier (floating point) of standard deviation. Default is 1.

Following is an example scheduler configuration.

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
      metricProvider:
      	type: Prometheus
      	address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
```
