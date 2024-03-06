# Overview

This folder holds system call-based scheduling (SySched) plugin implementations
based on [SySched](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/399-sysched-scoring).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
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
creating and storing system call usage profiles as seccomp profiles. We provide a quick installation guide herein.
For detailed instruction on installation and usage, please refer to the
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

The `SySched` plugin has its own specific parameters. Following is an example (`manifests/sysched/scheduler-config.yaml`) scheduler configuration for `SySched`.

```
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
leaderElection:
  # (Optional) Change true to false if you are not running a HA control-plane.
  leaderElect: true
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
  plugins:
    score:
      enabled:
      - name: SySched
  pluginConfig:
    - name: SySched
      args:
        defaultProfileNamespace: "default"
        defaultProfileName: "full-seccomp"
```

### Demo
Let assume a Kubernetes cluster with two worker nodes and a master node as follows. We also assume that the
`Security Profile Operator` and the Kubernetes `default-scheduler` with our plugin `SySched` enabled
are installed in the cluster. We want to deploy two instances `nginx`, one instance of `memcached`, and one instance
of `redis` containers (i.e., pods) using the `default-scheduler`.

```
$ kubectl get nodes

NAME          STATUS   ROLES           AGE   VERSION
k8s-master2   Ready    control-plane   17d   v1.25.7
mynode-1      Ready    <none>          15d   v1.25.7
mynode-2      Ready    <none>          15d   v1.25.7
```

To deploy these pods using the `default-scheduler` with the `SySched` plugin enabled, we need to provide the
system call profile as the seccomp profile for each pod. As described above, we utilize `SPO` for creating and attaching
the seccomp profile as a CRD for a pod. Following steps show the creation of the seccomp profile and binding CRDs, and
the deployment yaml for `nginx`:


#### Step 1: Creating seccomp profile CRD using SPO

The following yaml file shows an example seccomp profile CRD specification for `nginx` using SPO. The system call list
is truncated in the CRD. The complete CRD is available in the `manifests/sysched/examples` directory.

``` 
$ cat manifests/sysched/examples/nginx.profile.yaml

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
kubectl apply -f manifests/sysched/examples/nginx.profile.yaml
kubectl apply -f manifests/sysched/examples/memcached.profile.yaml
kubectl apply -f manifests/sysched/examples/redis.profile.yaml
```

SySched also requires a full and unconfined system call profile for the pods that do
not have any system call profile specified. By default, SySched expects a SPO system
call profile CRD named `full-profile` in the `default` namespace. The CRD name and namespace
can also be set through the plugin configuration in the scheduler configuration yaml file.
To create the full system call profile, issue the following command.

```
kubectl apply -f manifests/sysched/examples/full.profile.yaml
```



To see the created seccomp profiles, please issue the following command: `kubectl get seccompprofiles`

```
NAME                STATUS      AGE
full-seccomp        Installed   55m
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
$ cat manifests/sysched/examples/nginx.binding.yaml

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
NOTE: In order for the binding webhook to work, one must need to label the namespace (for the seccomp profile and 
binding CRDs) using `kubectl label` command as follows. The `default` namespace is used here.

``kubectl label ns default spo.x-k8s.io/enable-binding=``

Following commands create the binding CRDs for `nginx`, `memcached`, and `redis` with their corresponding seccomp
profile CRDs.

```
kubectl apply -f manifests/sysched/examples/nginx.binding.yaml
kubectl apply -f manifests/sysched/examples/memcached.binding.yaml
kubectl apply -f manifests/sysched/examples/redis.binding.yaml
```

To see the created profile binding CRDs, please issue the following command: `kubectl get profilebindings`.

```
NAME                AGE
memcached-binding   56m
nginx-binding       56m
redis-binding       56m
```

#### Step 3: Creating deployments
The third step is to create deployments. The following yaml file (`nginx1.yaml`) shows the deployment
yaml for `nginx` where the scheduler the `default-scheduler` with `SySched` plugin enabled. If the
`SySched` plugin is enabled and used through a secondary scheduler please specify the `schedulerName`
field in the specification.

```
$ cat manifests/sysched/examples/nginx1.yaml

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
instance of `redis`. Please note that the order of the pod deployment may result in different placement of pods.

```
kubectl apply -f manifests/sysched/examples/nginx1.yaml
kubectl apply -f manifests/sysched/examples/redis1.yaml
kubectl apply -f manifests/sysched/examples/nginx2.yaml
kubectl apply -f manifests/sysched/examples/memcached1.yaml
```

To see the placement result, please issue the following command: `kubectl get pods -o wide`

```
NAME                          READY   STATUS    RESTARTS   AGE   IP            NODE       NOMINATED NODE   READINESS GATES
memcached1-5fd5d4cf7f-j5gvr   1/1     Running   0          4s    10.244.1.86   mynode-1   <none>           <none>
nginx1-b86d6f76c-bg4hl        1/1     Running   0          40s   10.244.2.93   mynode-2   <none>           <none>
nginx2-d678b7967-9jrb5        1/1     Running   0          18s   10.244.2.94   mynode-2   <none>           <none>
redis1-54fcc8f949-5ss5k       1/1     Running   0          32s   10.244.1.85   mynode-1   <none>           <none>
```

The order of the deployments here is `nginx1`, `redis1`, `nginx2`, and `memcached1`. The instance of `nginx1` could be deployed any nodes since initially both nodes are empty. In this scenario, `nginx1` has been deployed to `mynode-2`. When deploying `redis1`, the scheduler will choose `mynode-1` as this will minimize the ExS score. Similarly for deploying `nginx2`, selecting `mynode-2` will result in lower ExS score as opposed to selecting `mynode-1`. Finally for `memcached1` will be placed to `mynode-1` due to the fact that `redis1` and `memcached1` have similar system call profile. Hence, placing together in a node will result in lower ExS score.
