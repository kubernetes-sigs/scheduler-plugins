# Scheduler Plugins

asdf

Repository for out-of-tree scheduler plugins based on scheduler framework.

## Install

Starting with release v0.18.9 container images are available in the official k8s container registry
`k8s.gcr.io/scheduler-plugins/kube-scheduler`. See the [Compatibility Matrix section](#compatibility-matrix)
for the list of images.

```shell
docker pull k8s.gcr.io/scheduler-plugins/kube-scheduler:$TAG
```

In a future release an official container image will be provided for the scheduler-plugins controller. For
example `docker pull k8s.gcr.io/scheduler-plugins/controller:$TAG`. 
You can find [how to install release image](doc/install.md) here.

## Compatibility Matrix
The below compatibility matrix shows the k8s client package(client-go, apimachinery, etc) versions that the
scheduler-plugins are compiled with.

The minor version of the scheduler-plugins matches the minor version of the k8s client
packages that it is compiled with. For example scheduler-plugins v0.18.x releases are built with k8s v1.18.x
dependencies.

The scheduler-plugins patch versions come in two different varieties(single digit or three digits). The single digit
patch versions(i.e. v0.18.9) exactly align with the the k8s client package versions that the scheduler plugins are built
with. The three digit patch versions(i.e. v0.18.800) are used to indicated that the k8s client package versions have not
changed since the previous release, and that only scheduler plugins code(features or bug fixes) was changed.

Scheduler Plugins  | Compiled With k8s Version | Container Image                                     |
-------------------|---------------------------|-----------------------------------------------------|
v0.18.9            | v1.18.9                   | k8s.gcr.io/scheduler-plugins/kube-scheduler:v0.18.9 |

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://kubernetes.slack.com/messages/sig-scheduling)
- [Mailing List](https://groups.google.com/forum/#!forum/kubernetes-sig-scheduling)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
