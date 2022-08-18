# Scheduler Plugins

Repository for out-of-tree scheduler plugins based on the [scheduler framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/).

This repo provides scheduler plugins that are exercised in large companies.
These plugins can be vendored as Golang SDK libraries or used out-of-box via the pre-built images or Helm charts.
Additionally, this repo incorporates best practices and utilities to compose a high-quality scheduler plugin.

## Install

Container images are available in the official scheduler-plugins k8s container registry. There are two images one
for the kube-scheduler and one for the controller. See the [Compatibility Matrix section](#compatibility-matrix)
for the complete list of images.

```shell
docker pull k8s.gcr.io/scheduler-plugins/kube-scheduler:$TAG
docker pull k8s.gcr.io/scheduler-plugins/controller:$TAG
```

You can find [how to install release image](doc/install.md) here.

## Plugins

The kube-scheduler binary includes the below list of plugins. They can be configured by creating one or more
[scheduler profiles](https://kubernetes.io/docs/reference/scheduling/config/#multiple-profiles).

* [Capacity Scheduling](pkg/capacityscheduling/README.md)
* [Coscheduling](pkg/coscheduling/README.md)
* [Node Resources](pkg/noderesources/README.md)
* [Node Resource Topology](pkg/noderesourcetopology/README.md)
* [Preemption Toleration](pkg/preemptiontoleration/README.md)
* [Trimaran](pkg/trimaran/README.md)

Additionally the kube-scheduler binary includes the below list of sample plugins. These plugins are not intended for use in production
environments.

* [Cross Node Preemption](pkg/crossnodepreemption/README.md)
* [Pod State](pkg/podstate/README.md)
* [Quality of Service](pkg/qos/README.md)

## Compatibility Matrix

The below compatibility matrix shows the k8s client package (client-go, apimachinery, etc) versions
that the scheduler-plugins are compiled with.

The minor version of the scheduler-plugins matches the minor version of the k8s client packages that
it is compiled with. For example scheduler-plugins `v0.18.x` releases are built with k8s `v1.18.x`
dependencies.

The scheduler-plugins patch versions come in two different varieties (single digit or three digits).
The single digit patch versions (e.g., `v0.18.9`) exactly align with the the k8s client package
versions that the scheduler plugins are built with. The three digit patch versions, which are built
on demand, (e.g., `v0.18.800`) are used to indicated that the k8s client package versions have not
changed since the previous release, and that only scheduler plugins code (features or bug fixes) was
changed.

| Scheduler Plugins | Compiled With k8s Version | Container Image                                      | Arch           |
|-------------------|---------------------------|------------------------------------------------------|----------------|
| v0.23.10          | v1.23.10                  | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.23.10 | AMD64<br>ARM64 |
| v0.22.6           | v1.22.6                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.22.6  | AMD64<br>ARM64 |
| v0.21.6           | v1.21.6                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.21.6  | AMD64<br>ARM64 |
| v0.20.10          | v1.20.10                  | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.20.10 | AMD64<br>ARM64 |
| v0.19.9           | v1.19.9                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.19.9  | AMD64<br>ARM64 |
| v0.19.8           | v1.19.8                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.19.8  | AMD64<br>ARM64 |
| v0.18.9           | v1.18.9                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.18.9  | AMD64          |

| Controller | Compiled With k8s Version | Container Image                                  | Arch           |
|------------|---------------------------|--------------------------------------------------|----------------|
| v0.23.10   | v1.23.10                  | k8s.gcr.io/scheduler-plugins/controller:v0.23.10 | AMD64<br>ARM64 |
| v0.22.6    | v1.22.6                   | k8s.gcr.io/scheduler-plugins/controller:v0.22.6  | AMD64<br>ARM64 |
| v0.21.6    | v1.21.6                   | k8s.gcr.io/scheduler-plugins/controller:v0.21.6  | AMD64<br>ARM64 |
| v0.20.10   | v1.20.10                  | k8s.gcr.io/scheduler-plugins/controller:v0.20.10 | AMD64<br>ARM64 |
| v0.19.9    | v1.19.9                   | k8s.gcr.io/scheduler-plugins/controller:v0.19.9  | AMD64<br>ARM64 |
| v0.19.8    | v1.19.8                   | k8s.gcr.io/scheduler-plugins/controller:v0.19.8  | AMD64<br>ARM64 |

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-scheduling)
- [Mailing List](https://groups.google.com/forum/#!forum/kubernetes-sig-scheduling)

You can find an [instruction how to build and run out-of-tree plugin here](doc/develop.md) .

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
