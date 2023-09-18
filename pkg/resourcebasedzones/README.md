# Overview

This folder holds the ZoneResource plugin implementations.

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [x] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial
Requires working with Coscheduling plugin. Can't be a standalone.
The plugin recieves few arguments through the config:
- ZoneLabel - the label on the nodes to base the grouping of zone
- ResourceNamespace - the prefix of the devices, i.e habana.ai will look for pod requestes of
  devices habana.ai/gaudi, habana.ai/greco, etc...
- PriorityZones - an ordered list of scoring per zone.

Pods requires the annotations of 'habana.ai/strict-zone: true'

### Expectation

1. All members of the same PodGroup will be scheduled into the same zone
2. Nodes in zones will be prioritzed based on the order provided to the plugin

### Config

1. Filter plugin add-on
2. Score plugin add-on

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta3
        kind: KubeSchedulerConfiguration
        leaderElection:
          leaderElect: true
        clientConnection:
          kubeconfig: /etc/kubernetes/scheduler.conf
        profiles:
        - schedulerName: default-scheduler
          plugins:
            queueSort:
              enabled:
              - name: Coscheduling
              disabled:
              - name: "*"
            preFilter:
              enabled:
              - name: Coscheduling
            filter:
              enabled:
              - name: ZoneResource
            postFilter:
              enabled:
              - name: Coscheduling
            score:
              enabled:
              - name: ZoneResource
            permit:
              enabled:
              - name: Coscheduling
            reserve:
              enabled:
              - name: Coscheduling
            postBind:
              enabled:
              - name: Coscheduling
          pluginConfig:
          - name: ZoneResource
            args:
              ResourceNamespace: 'habana.ai'
              ZoneLabel: 'habana.ai/zone'
              PriorityZones: ["a", "b", "c"]
```

### Demo

Suppose we have a cluster which can only afford 3 nginx pods. We create a ReplicaSet with replicas=6, and set the value of minMember to 3.
```yaml
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: nginx
  annotaions:
    habana.ai/strict-zone: "true"
spec:
  minMember: 3
---
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: nginx
  labels:
    app: nginx
  annotaions:
    habana.ai/strict-zone: "true"
spec:
  replicas: 3
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
        pod-group.scheduling.sigs.k8s.io: nginx
    spec:
      containers:
      - name: nginx
        image: nginx
        resources:
          limits:
            cpu: 3000m
            memory: 500Mi
          requests:
            cpu: 3000m
            memory: 500Mi
```

```script
$ kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE  Zone
nginx-4jw2m   1/1     Pending   0          55s  a
nginx-4mn52   1/1     Running   0          55s  a
nginx-c9gv8   1/1     Running   0          55s  a
```