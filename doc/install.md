# Install Scheduler-plugins

## Table of Contents

- [Create a Kubernetes Cluster](#create-a-kubernetes-cluster)
- [Install release v0.18.9 and use Coscheduling](#install-release-v0189-and-use-coscheduling)
  - [Test Coscheduling](#test-coscheduling)
- [Uninstall Scheduler-plugins](#uninstall-scheduler-plugins)

## Create a Kubernetes Cluster

Firstly you need to have a Kubernetes cluster, and a `kubectl` command-line tool must be configured to communicate with the cluster.

The Kubernetes version must equal to or greater than **v1.18.0**. To check the version, use `kubectl version --short`.

If you do not have a cluster yet, create one by using one of the following provision tools:

* [kind](https://kind.sigs.k8s.io/docs/)
* [kubeadm](https://kubernetes.io/docs/admin/kubeadm/)
* [minikube](https://minikube.sigs.k8s.io/)

## Install release v0.18.9 and use Coscheduling

In this section, we will walk you through how to replace the default scheduler with the scheduler-plugins image. As the new image is built on top of the default scheduler, you won't lose any vanilla Kubernetes scheduling capability. Instead, a lot of extra out-of-box functionalities (implemented by the plugins in this repo) can be obtained, such as coscheduling.

1. Log on Master node 
   * If your cluster is created by `kind`
     ```bash
     sudo docker exec -it $(sudo docker ps | grep control-plane | awk '{print $1}') bash
     ```
  
2. Backup `kube-scheduler.yaml`

   ```bash
   cp /etc/kubernetes/manifests/kube-scheduler.yaml /etc/kubernetes/kube-scheduler.yaml
   ```

3. Create `/etc/kubernetes/coscheduling-config.yaml`

   ```yaml
   apiVersion: kubescheduler.config.k8s.io/v1alpha2
   kind: KubeSchedulerConfiguration
   leaderElection:
     # (Optional) Change true to false if you are not running a HA control-plane.
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
       permit:
         enabled:
         - name: Coscheduling
       unreserve:
         enabled:
         - name: Coscheduling
   ```

  4. Modify `/etc/kubernetes/manifests/kube-scheduler.yaml` to run Scheduler-plugins with Coscheduling

     Generally, we need to make a couple of changes:
     - pass in the composed scheduler-config file via argument `--config`
     - (optional) remove duplicated CLI parameters (e.g., `--leader-elect`), as they may have been defined in the config file
     - replace vanilla Kubernetes scheduler image with scheduler-plugin image
     - mount the scheduler-config file to be readable when scheduler starting

     ```diff
     --- /etc/kubernetes/kube-scheduler.yaml 2021-02-04 01:27:42.392508733 +0000
     +++ /etc/kubernetes/manifests/kube-scheduler.yaml       2021-02-04 04:26:04.459171135 +0000
     @@ -13,11 +13,12 @@
          - kube-scheduler
          - --authentication-kubeconfig=/etc/kubernetes/scheduler.conf
          - --authorization-kubeconfig=/etc/kubernetes/scheduler.conf
     +    - --config=/etc/kubernetes/coscheduling-config.yaml
          - --bind-address=127.0.0.1
          - --kubeconfig=/etc/kubernetes/scheduler.conf
          - --leader-elect=true
          - --port=0
     -    image: k8s.gcr.io/kube-scheduler:v1.19.1
     +    image: k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.18.9
          imagePullPolicy: IfNotPresent
          livenessProbe:
            failureThreshold: 8
     @@ -47,6 +48,9 @@
          - mountPath: /etc/kubernetes/scheduler.conf
            name: kubeconfig
            readOnly: true
     +    - mountPath: /etc/kubernetes/coscheduling-config.yaml
     +      name: coscheduling-config
     +      readOnly: true
        hostNetwork: true
        priorityClassName: system-node-critical
        volumes:
     @@ -54,4 +58,8 @@
            path: /etc/kubernetes/scheduler.conf
            type: FileOrCreate
          name: kubeconfig
     +  - hostPath:
     +      path: /etc/kubernetes/coscheduling-config.yaml
     +      type: File
     +    name: coscheduling-config
      status: {}
     ```
   
4. Verify that kube-scheduler pod is running properly with a correct image: `k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.18.9`

   ```bash
   $ kubectl get pod -n kube-system | grep kube-scheduler
   kube-scheduler-xqcluster-control-plane            1/1     Running   0          3m27s

   $ kubectl get pods -l component=kube-scheduler -n kube-system -o=jsonpath="{.items[0].spec.containers[0].image}"
   k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.18.9
   ```
### Test Coscheduling

Assume there is 1500m free cpu resource in the cluster.

1. Create a replicaset with `pod-group.scheduling.sigs.k8s.io/name: nginx` and `pod-group.scheduling.sigs.k8s.io/min-available: "4"`. Each pod acquires 500m cpu.

   ```yaml
   apiVersion: apps/v1
   kind: ReplicaSet
   metadata:
     name: nginx
     namespace: default
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
           pod-group.scheduling.sigs.k8s.io/name: nginx
           pod-group.scheduling.sigs.k8s.io/min-available: "4"
       spec:
         containers:
         - name: nginx
           image: nginx
           imagePullPolicy: IfNotPresent
           resources:
             limits:
               cpu: 500m
             requests:
               cpu: 500m
   ```

2. Check pods

   All nginx pods are expected to be `Pending` as they cannot be co-scheduled altogether.

   ```bash
   $ kubectl get pod -n default
   NAME          READY   STATUS    RESTARTS   AGE
   nginx-25797   0/1     Pending   0          2m
   nginx-6fdcm   0/1     Pending   0          2m
   nginx-stcxq   0/1     Pending   0          2m
   nginx-xn25q   0/1     Pending   0          2m
   ```

## Uninstall Scheduler-plugins

1. Delete ReplicaSet
   ```bash
   kubectl delete replicaset -n default nginx
   ```

2. Recover `kube-scheduler.yaml` and delete `coscheduling-config.yaml`

   * If the cluster is created by `kubeadm` or `minikube`, log into Master node:
       ```bash
       mv /etc/kubernetes/kube-scheduler.yaml /etc/kubernetes/manifests/
       rm /etc/kubernetes/coscheduling-config.yaml
       ```

   * If the cluster is created by `kind`, enter the Master's container:
       ```bash
       sudo docker exec -it $(sudo docker ps | grep control-plane | awk '{print $1}') bash
       mv /etc/kubernetes/kube-scheduler.yaml /etc/kubernetes/manifests/
       rm /etc/kubernetes/coscheduling-config.yaml
       exit
       ```

4. Check default scheduler state

   ```bash
   $ kubectl get pod -n kube-system | grep kube-scheduler
   kube-scheduler-xqcluster-control-plane            1/1     Running   0          91s
   ```

