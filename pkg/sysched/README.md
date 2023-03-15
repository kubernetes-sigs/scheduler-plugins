# Overview

This folder holds system call-based scheduling (SySched) plugin implementations
based on [SySched](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/399-sysched-scoring).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [x ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial

### Expectation

The system call aware scheduler ([SySched](https://github.com/mvle/scheduler-plugins/tree/master/kep/399-sysched-scoring)) 
plugin improves the ranking of feasible nodes based on the relative risks of pods' system call usage to improve pods' 
security footprints. The system call usage profiles are stored as CRDs. 
The [Security Profile Operator (SPO)](https://github.com/kubernetes-sigs/security-profiles-operator) for creating and 
storing system call usage profiles as seccomp profiles. SyShed obtains the system call profile(s) for a pod from the 
CRDs and computes a score for Extraneous System Call (ExS). The normalized ExS score is combined with other scores in 
Kubernetes for ranking candidate nodes. 


### Installation of Security Profile Operator
We leverage the [Security Profile Operator (SPO)](https://github.com/kubernetes-sigs/security-profiles-operator) for
creating and storing system call usage profiles as seccomp profiles. We provide a very brief overview for installing
the SPO in a Kubernetes cluster. For detailed instruction on installation and usage, please refer to the
[SPO](https://github.com/kubernetes-sigs/security-profiles-operator) GitHub repository.

First, install the `cert-manager` via `kubectl` to deploy the SPO operator:
```
$ kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.11.0/cert-manager.yaml
$ kubectl --namespace cert-manager wait --for condition=ready pod -l app.kubernetes.io/instance=cert-manager
```

Next, we need to apply the operator manifest:
```
$ kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/security-profiles-operator/main/deploy/operator.yaml
```

### Scheduler configuration 
[***TODO: need to updated args***]


The `SySched` plugin may have its own specific parameters. Following is an example scheduler configuration 
for `SySched`.
```
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: sysched-scheduler
  plugins:
    score:
      enabled:
      - name: SySched
  pluginConfig:
    - name: TargetLoadPacking
      args:
        defaultRequestsMultiplier: "1"
        targetUtilization: 40
        metricProvider:
          type: Prometheus
          address: "http://replace_me_with_prometheus_server:9090"
```

### Demo
Let assume a Kubernetes cluster with two worker nodes and a master node as follows. We also assume that the 
`Security Profile Operator` and the Kubernetes scheduler named `sysched-scheduler` with our plugin `SySched` enabled
are installed in the cluster. We want to deploy two instances `nginx`, one instance of `memcached`, and one instance
of `redis` containers (i.e., pods) using the `sysched-scheduler` scheduler.

```
$ kubectl get nodes

NAME          STATUS   ROLES           AGE   VERSION
k8s-master2   Ready    control-plane   17d   v1.24.9
mynode-1      Ready    <none>          15d   v1.24.9
mynode-2      Ready    <none>          15d   v1.24.9
```

To deploy these pods using the `sysched-scheduler` with the `SySched` plugin enabled, we need to provide the 
system call profile as the seccomp profile for each pod. As described above, we utilize `SPO` for creating and attaching
the seccomp profile as a CRD for a pod. Following steps show the creation of the seccomp profile and binding CRDs, and 
the deployment yaml for `nginx`:


#### Step 1: Creating seccomp profile CRD using SPO

The following yaml file shows an example seccomp profile CRD specification for `nginx` using SPO. The system call list is truncated 
in the CRD. The complete CRD is available in [some path](***).

``` 
$ cat nginx.profile.yaml

---
apiVersion: security-profiles-operator.x-k8s.io/v1beta1
kind: SeccompProfile
metadata:
  name: nginx-seccomp
  namespace: default
spec:
  defaultAction: SCMP_ACT_LOG
  architectures:
  - SCMP_ARCH_X86_64
  syscalls:
  - action: SCMP_ACT_ALLOW
    names:
    - pivot_root
    - listen
    - statfs
    - clone
    - setuid
    ...
```
Following commands create the seccomp profile CRDs for `nginx`, `memcached`, and `redis`.
```
kubectl apply -f nginx.profile.yaml
kubectl apply -f memcached.profile.yaml
kubectl apply -f redis.profile.yaml
```

To see the created seccomp profiles, please issue the following command: `kubectl get seccompprofiles`

```
NAME                STATUS      AGE
memcached-seccomp   Installed   55m
nginx-seccomp       Installed   55m
redis-seccomp       Installed   55m
```

#### Step 2: Creating a binding CRD for dynamically attaching profile CRD to one or more pods
One can create a profile CRD and attached the profile to a pod using the `securityContext` field in the pod
specification. A better way to do it is by creating a binding CRD that attaches a seccomp profile CRD to one or 
more pods. The following yaml file shows an example profile binding CRD specification for `nginx` using SPO. 
The binding CRD attaches the seccomp profile CRD `nginx-seccomp` (created in Step 1) with all `nginx` pods with the
container image tag `nginx:1.16`.


```
$ cat nginx.binding.yaml

---
apiVersion: security-profiles-operator.x-k8s.io/v1alpha1
kind: ProfileBinding
metadata:
  name: nginx-binding
  namespace: default
spec:
  profileRef:
    kind: SeccompProfile
    name: nginx-seccomp
  image: nginx:1.16
```

Following commands create the binding CRDs for `nginx`, `memcached`, and `redis` with their corresponding seccomp
profile CRDs.

```
kubectl apply -f nginx.binding.yaml
kubectl apply -f memcached.binding.yaml
kubectl apply -f redis.binding.yaml
```

To see the created profile binding CRDs, please issue the following command: `kubectl get profilebindings`.

```
NAME                AGE
memcached-binding   56m
nginx-binding       15d
redis-binding       56m
```

#### Step 3: Creating a deployment specifying the scheduler name
The third step is to create a deployment by specifying the scheduler name. The following yaml file (`nginx1.yaml`) shows the deployment
yaml for `nginx` where the scheduler name is `sysched-scheduler`.

```
$ cat nginx1.yaml

apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx1
  labels:
    app: nginx1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx1
  template:
    metadata:
      labels:
        app: nginx1
    spec:
      serviceAccountName: default
      schedulerName: sysched-scheduler
      containers:
      - name: nginx1 
        image: nginx:1.16
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: nginx1
spec:
  type: NodePort
  selector:
    app: nginx1
  ports:
    - protocol: TCP
      port: 80
```

The following commands create the deployments for two instance of `nginx`, one instance of `memcached`, and one
instance of `redis`. Please note that the order of the pod deployment may result different placement of pods. 

```
kubectl apply -f nginx1.yaml
kubectl apply -f memcached1.yaml
kubectl apply -f redis1.yaml
kubectl apply -f nginx2.yaml
```

To see the placement result, please issue the following command: `kubectl get pods -o wide`

```
NAME                         READY   STATUS    RESTARTS   AGE   IP            NODE       NOMINATED NODE   READINESS GATES
memcached1-c679d8798-z6mbg   1/1     Running   0          6s    10.244.1.66   mynode-1   <none>           <none>
nginx1-bc7bd66b6-tqp5w       1/1     Running   0          8s    10.244.2.69   mynode-2   <none>           <none>
nginx2-5c6c479bbb-5zpff      1/1     Running   0          7s    10.244.2.70   mynode-2   <none>           <none>
redis1-587556f44-l7csx       1/1     Running   0          8s    10.244.1.65   mynode-1   <none>           <none>
```
