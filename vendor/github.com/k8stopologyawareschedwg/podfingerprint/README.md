[![Go Reference](https://pkg.go.dev/badge/github.com/k8stopologyawareschedwg/podfingerprint.svg)](https://pkg.go.dev/github.com/k8stopologyawareschedwg/podfingerprint)

# podfingerprint: compute the fingerprint of a set of pods

This package computes the fingerprint of a set of [kubernetes pods](https://kubernetes.io/docs/concepts/workloads/pods).
For the purposes of this package, a Pod is only its namespace + name pair, used to identify it.
A "fingerprint" is a compact unique representation of this set of pods.
Any given unordered set of pods with the same elements will yield the same fingerprint, regardless of the order on which the pods are enumerated.
The fingerprint is not actually unique because it is implemented using a hash function, but the collisions are expected to be extremely low.
Note this package will *NOT* restrict itself to use only cryptographically secure hash functions, so you should NOT use the fingerprint in security-sensitive contexts.

## LICENSE

apache v2

## known issues and limitations

### fingerprint v1: pod aliasing issue

the pod fingerprinting algorithm v1 uses the `namespace+name` pair to identify a pod. In case of high pod churn when burstable or guaranteed pods get deleted and
recreated with non-identical pod specs (e.g. changing pod resources requests/limits), the fingerprint will clash. So, two different set of pods will yield
the same fingerprint. [Future versions of the fingerprint will address this issue](https://github.com/k8stopologyawareschedwg/podfingerprint/issues/3).

#### mitigations

Kubernetes best practices [suggest to always use controllers, not "naked" pods](https://kubernetes.io/docs/concepts/configuration/overview/#naked-pods-vs-replicasets-deployments-and-jobs).
Kubernetes controllers [will generate create unique names for pods](https://github.com/kubernetes/kubernetes/blob/v1.24.1/pkg/controller/controller_utils.go#L553),
hence in this scenario the name clash described above should occur very rarely.

#### more details

The v1 fingerprint algorithm uses the `namespace+name` pair to identify a pod because of a conscious design decision due to a survey of the data source available
to the perspective consumers of this package.
Considering that the source of truth for resource allocation is the kubelet, and preferring node-local APIs, the agents running on the node have only few
data source options:
- the `pods` kubelet endpoint: reports the full pod spec, but does not report the resource allocation.
- the `podresources` kubelet endpoint: to learn about resource allocation, but does not report the pod UID, only the `namespace+name` pair.
Designing the v1 algo, we decided to create an API which we can used with _only_ the data provided by the `podresources` endpoint.
This is meant to avoid the perspective consumers of the package to avoid the impose the requirement to join these two data sources, preventing races and complications.

