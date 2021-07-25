# Pod Allocatable IP<!-- omit in toc -->

## Table of Contents <!-- omit in toc -->
<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
- [Use Cases](#use-cases)
- [Design Details](#design-details)
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

This plugin takes node's `.spec.PodCIDR` into account in scheduling cycle. When the density of pods in a node is relatively high, nodes might not have sufficient allocatable Pod IP to allocate.
Even though kubelet has its own `--max-pods` option flag to limit the total number of Pods on nodes, we can filter those nodes who have insufficient Pod IP to allocate in scheduling cycle. 

## Motivation
[kube-controller-manager](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/) provides the `--allocate-node-cidrs` options to mark Pod CIDR in each node's `.spec.PodCIDR` field.
When a node has already allocated all the Pod IPs from `.spec.PodCIDR`, a scheduling Pod will ignore this node in scheduling cycle. It avoids the situation that pods are bound to nodes without insufficient Pod IPs,
which is useful when a Pod cannot be scheduled due to the limited Pod IP allocation constraint in one node. This constraint is likely to result in Pods stucking in `ContainerCreating` phase.

### Goals

1. Implement a Filter plugin to filter out the nodes who have already allocated all the Pod IPs from `.spec.PodCIDR`.
2. This plugin only works for those clusters who use standard CNI plugins and enable the `--allocate-node-cidrs` options in [kube-controller-manager](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/), which means all pod IPs are strictly allocated from each node's `.spec.PodCIDR`.

### Non-Goals

1. Considering each node's Pod Allocatable IP in those clusters who use Elastic Network Interface(ENI) for Pod.
2. Considering each node's Pod Allocatable IP in those clusters whose `--allocate-node-cidrs` options of kube-controller-manager is disabled, which means the PodCIDR is not recorded into node resources.

## Use Cases

Make sure to set the `--allocate-node-cidrs=true --cluster-cidr=<cidr>` options in kube-controller-manager and use standard [CNI plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/network-plugins/). Each node would be assigned an IP subnet through the `--cluster-cidr=<cidr>` configuration.
For the node resources, `.spec.PodCIDR` field will be set as well.

## Design Details

We implement a filter plugin based on the scheduler framework. 

After nodes were joining into the cluster, their Pod IP subnet was assigned, and the `.spec.PodCIDR` field in node resources was set as well.

When a Pod is in scheduling cycle, the Pod Allocatable IP Filter Plugin will check if nodes has sufficient allocatable Pod IP to assign according its `.spec.PodCIDR` field.

Comparing each node's `.spec.PodCIDR` field and Pod IPs that have already been assigned, the Plugin calculates the number of allocatable Pod IP, filters out those nodes who have insufficient allocatable Pod IP to assign.

## Implementation History
