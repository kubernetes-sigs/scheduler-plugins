# Scheduler-plugins as a second scheduler in cluster

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

| Parameter                               | Description                                                                                                                               | Default                                                 |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| `scheduler.name`                        | Scheduler name                                                                                                                            | `scheduler-plugins-scheduler`                           |
| `scheduler.image`                       | Scheduler image                                                                                                                           | `k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.22.6`   |
| `scheduler.namespace`                   | Default scheduler-plugins namespace                                                                                                                       | `scheduler-plugins`                                     |
| `scheduler.replicaCount`                | scheduler-plugins replicas                                                                                                                    | `1`                                                     |
| `controller.name`                       | Controller name                                                                                                                           | `scheduler-plugins-controller`                          |
| `controller.image`                      | Controller image                                                                                                                          | `k8s.gcr.io/scheduler-plugins/controller:v0.22.6`       |
| `controller.namespace`                  | Controller namespace                                                                                                                      | `scheduler-plugins`                                     |    
| `controller.replicaCount`               | Controller replicaCount                                                                                                                   | `1`                                                     |
| `plugins.enabled`                       | All Plugins are enabled by default. Plugins enabled                                                                                                                           | `["Coscheduling","CapacityScheduling","NodeResourceTopologyMatch", "NodeResourcesAllocatable"]` |
| `global.queueSort`                      | The default `queueSort` Plugin, needs to be enabled once                                                                                       | `["Coscheduling"]`                                      |
| `global.extensions.preFilter`           | `preFilter` extension config. This is be used if the `preFilter` plugin is enabled.                                                                                                               | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.filter`              | `filter` extension config. This is be used if the `filter` plugin is enabled.                                                                                                                  | `["NodeResourceTopologyMatch"]`                         |
| `global.extensions.postFilter`          | `postFilter` extension config. This is be used if the `postFilter` plugin is enabled.                                                                                                              | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.score`               | `score` extension config. This is be used if the `score` plugin is enabled.                                                                                                                   | `["NodeResourceTopologyMatch", "NodeResourcesAllocatable"]` |
| `global.extensions.permit`              | `permit` extension config. This is be used if the `permit` plugin is enabled.                                                                                                                  | `["Coscheduling"]`                                      |
| `global.extensions.reserve`             | `reserve` extension config. This is be used if the `reserve` plugin is enabled.                                                                                                                 | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.postBind`            | `postBind` extension config. This is be used if the `postBind` plugin is enabled.                                                                                                                | `["Coscheduling"]`                                      |

