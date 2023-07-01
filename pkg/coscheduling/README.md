# Overview

This folder holds the coscheduling plugin implementations based on [Coscheduling based on PodGroup CRD](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/42-podgroup-coscheduling). The old version coscheduling is based on [Lightweight coscheduling based on back-to-back queue
 sorting](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/2-lightweight-coscheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [x] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial

### PodGroup

We use a special label named `scheduling.x-k8s.io/pod-group` to define a PodGroup. Pods that set this label and use the same value belong to the same PodGroup.

```
# PodGroup CRD spec
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: nginx
spec:
  scheduleTimeoutSeconds: 10
  minMember: 3
---
# Add a label `scheduling.x-k8s.io/pod-group` to mark the pod belongs to a group
labels:
  scheduling.x-k8s.io/pod-group: nginx
```

We will calculate the sum of the Running pods and the Waiting pods (assumed but not bind) in scheduler, if the sum is greater than or equal to the minMember, the Waiting pods
will be created.

Pods in the same PodGroup with different priorities might lead to unintended behavior, so need to ensure Pods in the same PodGroup with the same priority.

### Expectation

1. If 2 PodGroups with different priorities come in, the PodGroup with high priority has higher precedence.
2. If 2 PodGroups with same priority come in when there are limited resources, the PodGroup created first one has higher precedence.

### Config

1. queueSort, permit and unreserve must be enabled in coscheduling.
2. preFilter is enhanced feature to reduce the overall scheduling time for the whole group. It will check the total number of pods belonging to the same `PodGroup`. If the total number is less than minMember, the pod will reject in preFilter, then the scheduling cycle will interrupt. And the preFilter is user selectable according to the actual situation of users. If the minMember of PodGroup is relatively small, for example less than 5, you can disable this plugin. But if the minMember of PodGroup is relatively large, please enable this plugin to reduce the overall scheduling time.

```
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
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
    postFilter:
      enabled:
      - name: Coscheduling
    permit:
      enabled:
      - name: Coscheduling
    reserve:
      enabled:
      - name: Coscheduling
```

### Demo

Suppose we have a cluster which can only afford 3 nginx pods. We create a ReplicaSet with replicas=6, and set the value of minMember to 3.
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: nginx
spec:
  scheduleTimeoutSeconds: 10
  minMember: 3
---
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: nginx
  labels:
    app: nginx
spec:
  replicas: 6
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
        scheduling.x-k8s.io/pod-group: nginx
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
NAME          READY   STATUS    RESTARTS   AGE
nginx-4jw2m   0/1     Pending   0          55s
nginx-4mn52   1/1     Running   0          55s
nginx-c9gv8   1/1     Running   0          55s
nginx-frm24   0/1     Pending   0          55s
nginx-hsflk   0/1     Pending   0          55s
nginx-qtj5f   1/1     Running   0          55s
```

If minMember is set to 4 at this time, all nginx pods are in pending state because the resource does not meet the requirements of minMember
```script
$ kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE
nginx-4vqrk   0/1     Pending   0          3s
nginx-bw9nn   0/1     Pending   0          3s
nginx-gnjsv   0/1     Pending   0          3s
nginx-hqhhz   0/1     Pending   0          3s
nginx-n47r7   0/1     Pending   0          3s
nginx-n7vtq   0/1     Pending   0          3s
```
