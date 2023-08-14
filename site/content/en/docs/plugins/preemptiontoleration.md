# Overview

This folder holds the preemption toleration plugin implemented as discussed in [Preemption Toleration](../kep/205-preemption-toleration/README.md).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Example scheduler config:

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
      - name: PreemptionToleration
      disabled:
      - name: DefaultPreemption
```

## How to define PreemptionToleration policy on PriorityClass resource

Preemption toleration policy can be defined on each `PriorityClass` resource by annotations like below:

```yaml
# PriorityClass with PreemptionToleration policy:
# Any pod P in this priority class can not be preempted (can tolerate preemption)
# - by preemptor pods with priority < 10000
# - and if P is within 1h since being scheduled
kind: PriorityClass
metadata:
  name: toleration-policy-sample
  annotation:
    preemption-toleration.scheduling.x-k8s.io/minimum-preemptable-priority: "10000"
    preemption-toleration.scheduling.x-k8s.io/toleration-seconds: "3600"
value: 8000
```
