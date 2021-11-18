# Overview

This folder holds the node resources allocatable plugin implemented as discussed in [NodeResourcesLeastAllocatable as score plugin](https://github.com/kubernetes/kubernetes/issues/93547).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [x] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Node Resources Allocatable Plugin
### Resource Weights
Resources are assigned weights based on the plugin args resources param. The base units for CPU are millicores, while the base units for memory are bytes.

Example config:

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
    score:
      enabled:
      - name: NodeResourcesAllocatable
  pluginConfig:
  - name: NodeResourcesAllocatable
    args:
      mode: Least
      resources:
      - name: cpu
        weight: 1000000
      - name: memory
        weight: 1
```

### Node Resources Least Allocatable
If plugin args specify the priority param "Least", then nodes with the least allocatable resources are scored highest.

### Node Resources Most Allocatable
If plugin args specify the priority param "Most", then nodes with the most allocatable resources are scored highest.
