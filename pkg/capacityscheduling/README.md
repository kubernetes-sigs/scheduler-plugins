# Overview

This folder holds the capacity scheduling plugin implementations based on [Capacity Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/9-capacity-scheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [x] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial

Example config:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig:   "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: default-scheduler
  plugins:
    preFilter:
      enabled:
      - name: CapacityScheduling
    postFilter:
      enabled:
      - name: CapacityScheduling
      disabled:
      - name: "*"
    reserve:
      enabled:
      - name: CapacityScheduling
```

### ElasticQuota

```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota1
  namespace: quota1
spec:
  max:
    cpu: 6
  min:
    cpu: 4
```

- max: the upper bound of the resource consumption of the consumers.
- min: the minimum resources that are guaranteed to ensure the basic functionality/performance of the consumers

### Demo

We assume two elastic quotas are defined: quota1 (min:`cpu 4`, max:`cpu 6`) and quota2 
(min:`cpu 4`, max:`cpu 6`). The entire cluster 
has 8 CPUs available hence the sum of quota min is equal to the cluster capacity.

- create namespace and ElasticQuota

```script
$ kubectl create ns quota1
$ kubectl create ns quota2
$ cat <<EOF | kubectl apply -f -
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota1
  namespace: quota1
spec:
  max:
    cpu: 6
  min:
    cpu: 4
EOF
$ cat <<EOF | kubectl apply -f -
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota2
  namespace: quota2
spec:
  max:
    cpu: 6
  min:
    cpu: 4
EOF
```
 
- create app1

```script
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota1
  labels:
    app: nginx
spec:
  replicas: 4
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx
        resources:
          limits:
            cpu: 2
          requests:
            cpu: 2
EOF
```

After the usage of quota1 reaches `min(4)`, there are still 4 cpus available in the cluster. So the rest of the pods are still being scheduled until it reaches the `max(6)`. Finally, there is one pending pod.

```script
$ kubectl get pods -n quota1
NAME          READY   STATUS    RESTARTS   AGE
nginx-27qr9   1/1     Running   0          32s
nginx-2mxdd   1/1     Running   0          32s
nginx-6gbgx   1/1     Running   0          32s
nginx-bxvcg   0/1     Pending   0          32s
```

- create app2

```script
$ cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota2
  labels:
    app: nginx
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx
        resources:
          limits:
            cpu: 2
          requests:
            cpu: 2
EOF
```

```script
$ kubectl get pods -n quota1
NAME          READY   STATUS        RESTARTS   AGE
nginx-2mxdd   1/1     Running       0          6m49s
nginx-6gbgx   1/1     Running       0          6m49s
nginx-6gbgx   0/1     Terminating   0          6m49s
nginx-bxvcg   0/1     Pending       0          6m49s

$ kubectl get pods -n quota2
NAME          READY   STATUS    RESTARTS   AGE
nginx-4z2zd   1/1     Running   0          81s
nginx-6dfn9   1/1     Running   0          81s
```

When app2 is created, there are 2 cpus available in the cluster. So one pod is scheduled successfully. But the usage of quota2 has not reached `min(4)`. Meanwhile, the usage of quota1 has exceeded `min(4)`. The 2nd pod belongs to quota2 will be able to preempt pod(s) belong to quota1 that have used excessive cpus. So the 2nd pod gets scheduled eventually, making the usage of quota2 `min(4)` with a cost of preempting one pod in quota1.
