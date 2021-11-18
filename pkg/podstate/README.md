# Overview

This folder holds a sample plugin implementation based 
on the number of terminating and nominated Pods on a Node.

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [x] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Pod State Plugin

This is a score plugin that takes terminating and nominated Pods into accounts in the following manner:
- the nodes that have more terminating Pods will get a higher score as those terminating Pods would be physically removed eventually from nodes
- the nodes that have more nominated Pods (which carry .status.nominatedNodeName) will get a lower score as the nominated nodes are supposed to accommodate some preemptor pod in a 
future scheduling cycle.

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
    score:
      enabled:
      - name: PodState
```