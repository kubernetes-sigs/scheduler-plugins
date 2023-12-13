
# KEP  PEAKS: Power and Energy Aware Scheduling

## Table of Contents

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1](#story-1)
    - [Story 2](#story-2)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  
<!-- /toc -->

## Summary

PEAKS (Power Efficiency Aware Kubernetes Scheduler) is a Kubernetes Scheduler plugin that aims to optimize the aggregate power consumption of the entire cluster during scheduling. It uses pre-trained Machine Learning models correlating Node Utilization with Power Consumption to predict the most suitable nodes for scheduling workloads. These predictions are based on the resource needs of incoming workloads and the real-time utilization of nodes within the cluster.

## Motivation

The Kubernetes scheduler framework supports multiple plugins, enabling the scheduling of pods on nodes to optimize various objectives. However, within the ecosystem, there lacks a solution specifically designed to optimize power efficiency while simultaneously fulfilling other scheduling goals.

A new plugin addresses this gap by incorporating power efficiency as a scheduling criterion alongside other objectives, such as optimizing resource allocation or adhering to specific topology requirements.

### Goals

1. Provide a configurable scheduling plugin to minimize the aggregate power consumption of the cluster.
2. Implement the aforementioned features as Score plugins.
3. Avoid altering the behavior of default score plugins unless it's necessary.

### Non Goals

1. De-scheduling resulting from unexpected outcomes (such as hot nodes or fragmentation) due to past scoring by plugins is not addressed in the initial design.
2. Memory, network, and disk utilization are not considered in the initial design.
3. The migration of already scheduled pods running on less power-efficient nodes to more power-efficient nodes is out of scope in the initial design.
4. Shutting down nodes to optimize power by migrating already running pods to other cluster nodes is out of scope in the initial design."

## Proposal

### User Stories

#### Story 1

With 'heterogeneous nodes' (variations in CPU architectures, a mixed cluster consisting of both VM and Bare Metal nodes, and dissimilar allocation of resources across nodes), cluster owners can reap benefits, as the power efficiency of different nodes may vary. This often necessitates the use of customized power models for each node within the cluster.

#### Story 2

Even with 'homogeneous nodes,' if the CPU vs. Power relationship is non-linear (such as piece-wise linear or concave), cluster owners can still benefit. Although the power efficiency might be consistent across all nodes, the varying CPU utilizations among nodes at any given time can significantly impact the aggregate power consumption of the cluster. Placing an incoming pod on one node versus another can influence this consumption.

### Notes/Constraints/Caveats

- The [Min, Max] score range for the plugin is user-configurable, defaulting to [0, 100].
- Additionally, the plugin normalizes the generated scores within the supplied [Min, Max] range.
- There isn't a singular model that comprehensively depicts the correlation between utilization and power consumption for every cluster node.
- At any given time, the current utilization of a node can be identified through metrics data.
- Determining the current power consumption can be achieved either through live metrics or by inference from the model that describes the relationship between node utilization and power consumption.
- Retraining the model representing this relationship may be necessary to better align with the workloads running on the node.

### Risks and Mitigations

- The benchmark test below captures the overhead resulting from model inferencing latency, which appears to be negligible.
- The accuracy of the model that captures the relationship between utilization and power relies heavily on the quality of the metrics used for its training. The current implementation supports integration with various user-selected metrics providers, such as node-metrics and Kepler

## Design Details

[WIP]

Benchmarks

1. Peak Scheduler plugin with loadaware + default
    - latency in scheduling (script to get delta (time of request of workload, time of scheduling)
        - read pod events and cature the difference
    - compare peaks resource impact on the cluster
        - Capture cluster total cpu usage + memory : Load watcher + plugin + workload (stuck at lw)

2. Impact of scheduler:
   - measure workload resource cons Peaks vs loadaware, default
        - time of execution
        - resource consumption inc or dec because of cluster packing algo 