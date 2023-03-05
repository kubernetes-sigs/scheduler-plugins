# Scheduler-plugins as a second scheduler in cluster

## Table of Contents

<!-- toc -->
- [Installation](#installation)
  - [Prerequisites](#prerequisites)
  - [Installing the chart](#installing-the-chart)
    - [Install chart using Helm v3.0+](#install-chart-using-helm-v30)
    - [Verify that scheduler and plugin-controller pod are running properly.](#verify-that-scheduler-and-plugin-controller-pod-are-running-properly)
  - [Configuration](#configuration)
  - [Configure bin-packing in Helm chart](#configure-bin-packing-in-helm-chart)
<!-- /toc -->

## Installation

Quick start instructions for the setup and configuration of as-a-second-scheduler using Helm.

### Prerequisites

- [Helm](https://helm.sh/docs/intro/quickstart/#install-helm)

### Installing the chart

#### Install chart using Helm v3.0+

```bash
$ git clone git@github.com:kubernetes-sigs/scheduler-plugins.git
$ cd scheduler-plugins/manifests/install/charts
$ helm install scheduler-plugins as-a-second-scheduler/
```

#### Verify that scheduler and plugin-controller pod are running properly.

```bash
$ kubectl get deploy -n scheduler-plugins
NAME                           READY   UP-TO-DATE   AVAILABLE   AGE
scheduler-plugins-controller   1/1     1            1           7s
scheduler-plugins-scheduler    1/1     1            1           7s
```

### Configuration

The following table lists the configurable parameters of the as-a-second-scheduler chart and their default values.

| Parameter                 | Description                 | Default                                                                                         |
|---------------------------|-----------------------------|-------------------------------------------------------------------------------------------------|
| `scheduler.name`          | Scheduler name              | `scheduler-plugins-scheduler`                                                                   |
| `scheduler.image`         | Scheduler image             | `registry.k8s.io/scheduler-plugins/kube-scheduler:v0.24.9`                                      |
| `scheduler.namespace`     | Scheduler namespace         | `scheduler-plugins`                                                                             |
| `scheduler.replicaCount`  | Scheduler replicaCount      | `1`                                                                                             |
| `controller.name`         | Controller name             | `scheduler-plugins-controller`                                                                  |
| `controller.image`        | Controller image            | `registry.k8s.io/scheduler-plugins/controller:v0.24.9`                                          |
| `controller.namespace`    | Controller namespace        | `scheduler-plugins`                                                                             |
| `controller.replicaCount` | Controller replicaCount     | `1`                                                                                             |
| `plugins.enabled`         | Plugins enabled by default  | `["Coscheduling","CapacityScheduling","NodeResourceTopologyMatch", "NodeResourcesAllocatable"]` |
| `plugins.disabled`        | Plugins disabled by default | `["PrioritySort"]`                                                                              |

###  Configure bin-packing in Helm chart
If you want to use bin-packing in the helm chart, you need to add code similar to the following in the `pluginConfig` section of `values.yaml`.
```yaml
pluginConfig:
- name: Coscheduling
  args:
    permitWaitingTimeSeconds: 10 # default is 60
- name: NodeResourcesFit
  args:
    scoringStrategy:
      requestedToCapacityRatio:
        shape:
        - utilization: 0
          score: 0
        - utilization: 100
          score: 10
      resources:
      - name: intel.com/foo
        weight: 3
      - name: intel.com/bar
        weight: 3
      type: RequestedToCapacityRatio
```

Then the ConfigMap will be created.
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: scheduler-config
  namespace: scheduler-plugins
data:
  scheduler-config.yaml: |
    apiVersion: kubescheduler.config.k8s.io/v1beta3
    kind: KubeSchedulerConfiguration
    profiles:
      // Other codes here...
      ...
      pluginConfig:
      - args:
          permitWaitingTimeSeconds: 10
        name: Coscheduling
      - args:
          scoringStrategy:
            requestedToCapacityRatio:
              shape:
              - score: 0
                utilization: 0
              - score: 10
                utilization: 100
            resources:
            - name: intel.com/foo
              weight: 3
            - name: intel.com/bar
              weight: 3
            type: RequestedToCapacityRatio
        name: NodeResourcesFit
```