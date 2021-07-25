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

This plugin takes a node's `.spec.PodCIDR` into account in a scheduling cycle. When the density of pods in a node is relatively high, the node might not have sufficient allocatable Pod IPs to allocate.
Even though kubelet has its own `--max-pods` option flag to limit the total number of Pods on nodes, we can filter out those nodes who have insufficient Pod IPs to allocate in Pods' `ContainerCreating` phase. 

## Motivation
[kube-controller-manager](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/) provides the `--allocate-node-cidrs` options to mark Pod CIDR in each node's `.spec.PodCIDR` field.
When a node has already allocated all the Pod IPs from `.spec.PodCIDR`, a scheduling Pod will ignore this node in scheduling cycle. It avoids the situation that pods are bound to nodes without insufficient Pod IPs,
which is useful when a Pod cannot be scheduled due to the limited Pod IP allocation constraint in one node. Without it, a pod can be scheduled and then stuck in `ContainerCreating` phase.

### Goals

- Implement a Filter plugin to filter out the nodes who have already allocated all the Pod IPs from `.spec.PodCIDR`.

### Non-Goals

1. Considering each node's Pod Allocatable IP in those clusters who use Elastic Network Interface(ENI) for Pod.
2. Considering each node's Pod Allocatable IP in those clusters whose `--allocate-node-cidrs` options of kube-controller-manager is disabled, which means the PodCIDR is not recorded into node resources.

## Use Cases

Make sure to set the `--allocate-node-cidrs=true --cluster-cidr=<cidr>` options in kube-controller-manager and use standard [CNI plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/network-plugins/). Each node would be assigned an IP subnet through the `--cluster-cidr=<cidr>` configuration.
For the node resources, `.spec.PodCIDR` field will be set as well.

# Known limitations

- This plugin assumes that the clusters use standard CNI plugins and enable the `--allocate-node-cidrs` options in [kube-controller-manager](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/), which means all pod IPs are strictly allocated from each node's `.spec.PodCIDR`. 
- In alpha version, this plugin only support IPv4 Pod IP addresses.

## Design Details

We implement filter plugin and preBind plugin based on the scheduler framework.

After nodes were joining into the cluster, their Pod IP subnet was assigned, and the `.spec.PodCIDR` field in node resources was set as well.

When a Pod is in scheduling cycle, the Plugin will check if nodes has sufficient allocatable Pod IP to assign according its `.spec.PodCIDR` field.

In the scheduling cycle, the filter plugin compares each node's `.spec.PodCIDR` field and Pod IPs that have already been assigned, the Plugin calculates the number of allocatable Pod IP, filters out those nodes who have insufficient allocatable Pod IPs to assign.

In the binding cycle, the preBind plugin double check the number of allocatable Pod IPs in the chosen node to avoid the race conditions (e.g., A node is chosen for Pod A in a scheduling cycle. Before the Pod A's binding cycle, Pod B has been bound to this node in its own binding cycle, this could result in mis-calculation of allocatable IPs on the node)

The allocatable Pod IP calculation method is shown as follows:

1. Obtain the node's Pod IP subnet from its own `.spec.PodCIDR` field (e.g., subnet `192.168.1.0/29` has 6 valid IP to assign).
2. Obtain the number of Pods that have already been bound to this node. To achieve that, the plugin starts a client to list all the Pods that has been scheduled in this node, and then filter out those Pods in hostNetwork (e.g., there are 3 Pods have been bound to this node).
3. Calculate the number of allocatable Pod IP in this node. In this case, that will be 3 (e.g., 6 valid IP to assign from subnet `192.168.1.0/29`, 3 Pods have been bound to this node, 3 Pod IPs left for this node).

## Implementation History
