---
weight: 1
---

# Install Scheduler-plugins

## Table of Contents

<!-- toc -->
- [Create a Kubernetes Cluster](#create-a-kubernetes-cluster)
- [Install release v0.27.8 and use Coscheduling](#install-release-v0278-and-use-coscheduling)
  - [As a second scheduler](#as-a-second-scheduler)
  - [As a single scheduler (replacing the vanilla default-scheduler)](#as-a-single-scheduler-replacing-the-vanilla-default-scheduler)
- [Test Coscheduling](#test-coscheduling)
- [Install old-version releases](#install-old-version-releases)
- [Uninstall scheduler-plugins](#uninstall-scheduler-plugins)
<!-- /toc -->

## Create a Kubernetes Cluster

Firstly you need to have a Kubernetes cluster, and a `kubectl` command-line tool must be configured to communicate with the cluster.

The Kubernetes version must equal to or greater than **v1.23.0**. To check the version, use `kubectl version --short`.

If you do not have a cluster yet, create one by using one of the following provision tools:

* [kind](https://kind.sigs.k8s.io/docs/)
* [kubeadm](https://kubernetes.io/docs/reference/setup-tools/kubeadm/)
* [minikube](https://minikube.sigs.k8s.io/)

## Install release v0.27.8 and use Coscheduling

Note: we provide two ways to install the scheduler-plugin artifacts: as a second scheduler
and as a single scheduler. Their pros and cons are as below:

- **second scheduler:**
  - **pro**: it's easy to install by deploying the Helm chart
  - **con**: running multi-scheduler will inevitably encounter resource conflicts when the cluster is short of resources.

    Consider the scenario where multiple schedulers attempt to assign their pods simultaneously to a node which can only fit one of the pods.
    The pod that arrives later will be evicted by the kubelet, and hang there (without its `.spec.nodeName` cleared) until resources get released on the node.

    Running multiple schedulers, therefore, is not recommended in the production env. However, it's a good starting point to play with
    scheduler framework and exercise plugin development, no matter you're on managed or on-premise Kubernetes clusters.
- **single scheduler:**
  - **pro**: you will be using a unified scheduler and hence keep the resources conflict-free. It's recommended for the production env.
  - **con**: you have to have the privileges to manipulate the control plane, and at this moment, the installation is not fully automated (no Helm chart yet).

### As a second scheduler
The quickest way to try scheduler-plugins is to install it using helm chart as a second scheduler.
You can find the demo chart in [manifests/install/charts](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/manifests/install/charts). **But if in the production environment, it is recommended to replace the default-scheduler manually(as described in next section).**

[Install using Helm Chart](./installing-the-chart.md)

### As a single scheduler (replacing the vanilla default-scheduler)

A bit different from the automatic installation steps above,
using scheduler-plugins as a single scheduler needs some manual steps.

>The main obstacle here is that we need to reconfigure the vanilla scheduler, but it's challenging to get it automated
as how it's deployed varies a lot (i.e., deployment, static pod, or an executable binary managed by systemd).
Moreover, managed Kubernetes offerings may be cluster-specific that need extra configuration
and hence hard to be pipelined nicely.

In this section, we will walk you through how to replace the default scheduler with the
scheduler-plugins image. As the new image is built on top of the default scheduler, you won't lose
any vanilla Kubernetes scheduling capability. Instead, a lot of extra out-of-box functionalities
(implemented by the plugins in this repo) can be obtained, such as coscheduling.

> The following steps are based on a Kubernetes cluster created by Kind.

1. Log into the control plane node

    ```bash
    sudo docker exec -it $(sudo docker ps | grep control-plane | awk '{print $1}') bash
    ```

1. Backup `kube-scheduler.yaml`

   ```bash
   cp /etc/kubernetes/manifests/kube-scheduler.yaml /etc/kubernetes/kube-scheduler.yaml
   ```

1. Create `/etc/kubernetes/sched-cc.yaml`

    ```yaml
    apiVersion: kubescheduler.config.k8s.io/v1
    kind: KubeSchedulerConfiguration
    leaderElection:
      # (Optional) Change true to false if you are not running a HA control-plane.
      leaderElect: true
    clientConnection:
      kubeconfig: /etc/kubernetes/scheduler.conf
    profiles:
    - schedulerName: default-scheduler
      plugins:
        multiPoint:
          enabled:
          - name: Coscheduling
          disabled:
          - name: PrioritySort
    ```

1. **❗IMPORTANT**❗ Starting with release v0.19, several plugins (e.g., coscheduling) introduced CRD
   to optimize their design and implementation. And hence we need an extra step to:

    - apply extra RBAC privileges to user `system:kube-scheduler` so that the scheduler binary is
      able to manipulate the custom resource objects
    - install a controller binary managing the custom resource objects

    Next, we apply the compiled yaml located at [manifests/install/all-in-one.yaml](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/manifests/install/all-in-one.yaml).

    ```bash
    $ kubectl apply -f all-in-one.yaml
    ```

    After this step, a deployment called `scheduler-plugins-controller` is expected to run in
    namespace `scheduler-plugins`:

    ```bash
    $ kubectl get deploy -n scheduler-plugins
    NAME                           READY   UP-TO-DATE   AVAILABLE   AGE
    scheduler-plugins-controller   1/1     1            1           19h
    ```

1. **❗IMPORTANT**❗ Install the CRDs your workloads depend on.

    You can refer to each folder under [manifests/crds](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/manifests/crds) to obtain the CRD yaml for each
    plugin. Here we install coscheduling CRD:

    ```bash
    $ kubectl apply -f manifests/crds/scheduling.x-k8s.io_podgroups.yaml
    ```

1. Modify `/etc/kubernetes/manifests/kube-scheduler.yaml` to run scheduler-plugins with coscheduling

    Generally, we need to make a couple of changes:

    - pass in the composed scheduler-config file via argument `--config`
    - (optional) remove duplicated CLI parameters (e.g., `--leader-elect`), as they may have been defined in the config file
    - replace vanilla Kubernetes scheduler image with scheduler-plugin image
    - mount the scheduler-config file to be readable when scheduler starting

    Here is a diff:

    ```diff
    16d15
    <     - --config=/etc/kubernetes/sched-cc.yaml
    17a17,18
    >     - --kubeconfig=/etc/kubernetes/scheduler.conf
    >     - --leader-elect=true
    19,20c20
    <     image: registry.k8s.io/scheduler-plugins/kube-scheduler:v0.27.8
    ---
    >     image: registry.k8s.io/kube-scheduler:v1.27.8
    50,52d49
    <     - mountPath: /etc/kubernetes/sched-cc.yaml
    <       name: sched-cc
    <       readOnly: true
    60,63d56
    <   - hostPath:
    <       path: /etc/kubernetes/sched-cc.yaml
    <       type: FileOrCreate
    <     name: sched-cc
    ```

1. Verify that kube-scheduler pod is running properly with a correct image: `registry.k8s.io/scheduler-plugins/kube-scheduler:v0.27.8`

    ```bash
    $ kubectl get pod -n kube-system | grep kube-scheduler
    kube-scheduler-kind-control-plane            1/1     Running   0          3m27s

    $ kubectl get pods -l component=kube-scheduler -n kube-system -o=jsonpath="{.items[0].spec.containers[0].image}{'\n'}"
    registry.k8s.io/scheduler-plugins/kube-scheduler:v0.27.8
    ```

    > **⚠️Troubleshooting:** If the kube-scheudler is not up, you may need to restart kubelet service inside the kind control plane (`systemctl restart kubelet.service`)

## Test Coscheduling

Now, we're able to verify how the coscheduling plugin works.

1. Create a PodGroup custom object called `pg1`:

    ```yaml
    # podgroup.yaml
    apiVersion: scheduling.x-k8s.io/v1alpha1
    kind: PodGroup
    metadata:
      name: pg1
    spec:
      scheduleTimeoutSeconds: 10
      minMember: 3
    ```

    ```bash
    $ kubectl apply -f podgroup.yaml
    ```

1. Create a deployment labelled `scheduling.x-k8s.io/pod-group: pg1` to associated with PodGroup
   `pg1` created in the previous step.

    ```yaml
    # deploy.yaml
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: pause
    spec:
      replicas: 2
      selector:
        matchLabels:
          app: pause
      template:
        metadata:
          labels:
            app: pause
            scheduling.x-k8s.io/pod-group: pg1
        spec:
          containers:
          - name: pause
            image: registry.k8s.io/pause:3.6
    ```

    > **⚠️Note:️** If you are running scheduler-plugins as a second scheduler, you should explicitly
    > specify `.spec.schedulerName` to match the secondary scheduler name:
    > ```yaml
    > # deploy.yaml
    > ...
    > spec:
    >   ...
    >   template:
    >     spec:
    >       schedulerName: scheduler-plugins-scheduler
    > ```

1. As PodGroup `pg1` requires at least 3 pods to be scheduled all-together, and there are only 2 Pods
   so far, so it's expected to observer they are pending:

    All nginx pods are expected to be `Pending` as they cannot be co-scheduled altogether.

    ```bash
    $ kubectl get pod
    NAME                     READY   STATUS    RESTARTS   AGE
    pause-646dbcfb64-4zvt6   0/1     Pending   0          9s
    pause-646dbcfb64-8kpg4   0/1     Pending   0          9s
   ```

1. Now let's scale the deployment up to have 3 replicas, so as to qualify for `minMember`
   (i.e., 3) of the associated PodGroup:

    ```bash
    $ kubectl scale deploy pause --replicas=3
    deployment.apps/pause scaled
    ```

    And wait for a couple of seconds, it's expected to see all Pods get into running state:

    ```bash
    $ kubectl get pod
    NAME                     READY   STATUS    RESTARTS   AGE
    pause-646dbcfb64-4zvt6   1/1     Running   0          42s
    pause-646dbcfb64-8kpg4   1/1     Running   0          42s
    pause-646dbcfb64-npzcf   1/1     Running   0          8s
    ```

1. You can also get the PodGroup's spec via:

    ```bash
    $ kubectl get podgroup pg1 -o yaml
    apiVersion: scheduling.x-k8s.io/v1alpha1
    kind: PodGroup
    metadata:
      annotations:
        kubectl.kubernetes.io/last-applied-configuration: |
          {"apiVersion":"scheduling.x-k8s.io/v1alpha1","kind":"PodGroup","metadata":{"annotations":{},"name":"pg1","namespace":"default"},"spec":{"minMember":3,"scheduleTimeoutSeconds":10}}
      creationTimestamp: "2022-02-08T19:55:24Z"
      generation: 8
      name: pg1
      namespace: default
      resourceVersion: "6142"
      uid: b4ac3562-54ab-4c1e-89bb-541a81c6acce
    spec:
      minMember: 3
      scheduleTimeoutSeconds: 10
    status:
      phase: Running
      running: 3
      scheduleStartTime: "2022-02-08T19:55:24Z"
      scheduled: 3
    ```

> ⚠ NOTE: There are some UX issues need to be addressed in controller side -
> [#166](https://github.com/kubernetes-sigs/scheduler-plugins/issues/166).

## Install old-version releases

If you're running at v0.18.9, which doesn't depend on PodGroup CRD, you should refer to the
[install doc](https://github.com/kubernetes-sigs/scheduler-plugins/blob/release-1.18/doc/install.md) in
branch `release-1.18` for detailed installation instructions.

## Uninstall scheduler-plugins

1. Delete the deployment

    ```bash
    $ kubectl delete deploy pause -n default
    ```

2. Recover `kube-scheduler.yaml` and delete `sched-cc.yaml`

    - If the cluster is created by `kubeadm` or `minikube`, log into Master node:
        ```bash
        $ mv /etc/kubernetes/kube-scheduler.yaml /etc/kubernetes/manifests/
        $ rm /etc/kubernetes/sched-cc.yaml
        ```

    - If the cluster is created by `kind`, enter the Master's container:
        ```bash
        $ sudo docker exec -it $(sudo docker ps | grep control-plane | awk '{print $1}') bash
        $ mv /etc/kubernetes/kube-scheduler.yaml /etc/kubernetes/manifests/
        $ rm /etc/kubernetes/sched-cc.yaml
        exit
        ```

4. Check state of default scheduler

    ```bash
    $ kubectl get pod -n kube-system | grep kube-scheduler
    kube-scheduler-kind-control-plane            1/1     Running   0          91s
    ```
