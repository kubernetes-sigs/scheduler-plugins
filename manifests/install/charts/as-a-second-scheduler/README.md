# Chart to run scheduler plugin as a second scheduler in cluster.

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

### Configuration

The following table lists the configurable parameters of the as-a-second-scheduler chart and their default values.

| Parameter                               | Description                                                                                                                               | Default                                                 |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| `scheduler.name`                        | Scheduler name                                                                                                                            | `scheduler-plugins-scheduler`                           |
| `scheduler.image`                       | Scheduler image                                                                                                                           | `k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.22.6`   |
| `scheduler.namespace`                   | Scheduler namespace                                                                                                                       | `scheduler-plugins`                                     |
| `scheduler.replicaCount`                | Scheduler replicaCount                                                                                                                    | `1`                                                     |
| `controller.name`                       | Controller name                                                                                                                           | `scheduler-plugins-controller`                          |
| `controller.image`                      | Controller image                                                                                                                          | `k8s.gcr.io/scheduler-plugins/controller:v0.22.6`       |
| `controller.namespace`                  | Controller namespace                                                                                                                      | `scheduler-plugins`                                     |    
| `controller.replicaCount`               | Controller replicaCount                                                                                                                   | `1`                                                     |
| `plugins.enabled`                       | Plugins enabled                                                                                                                           | `["Coscheduling","CapacityScheduling","NodeResourceTopologyMatch", "NodeResourcesAllocatable"]` |
| `global.queueSort`                      | global queueSort, needs to be globally enabled once                                                                                       | `["Coscheduling"]`                                      |
| `global.extensions.preFilter`           | global extensions preFilter                                                                                                               | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.filter`              | global extensions filter                                                                                                                  | `["NodeResourceTopologyMatch"]`                         |
| `global.extensions.postFilter`          | global extensions postFilter                                                                                                              | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.score`               | global extensions score                                                                                                                   | `["NodeResourceTopologyMatch", "NodeResourcesAllocatable"]` |
| `global.extensions.permit`              | global extensions permit                                                                                                                  | `["Coscheduling"]`                                      |
| `global.extensions.reserve`             | global extensions reserve                                                                                                                 | `["Coscheduling", "CapacityScheduling"]`                |
| `global.extensions.postBind`            | global extensions postBind                                                                                                                | `["Coscheduling"]`                                      |

