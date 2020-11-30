# Memory bandwidth and LLC aware filtering and scoring

## Table of Contents

<!-- toc -->
- [RDT aware filtering and scoring](#RDT-aware-filtering-and-scoring)
  - [Table of Contents](#table-of-contents)
  - [Summary](#summary)
  - [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
  - [Use Cases](#use-cases)
  - [Terms](#terms)
  - [Proposal](#proposal)
  - [Design Details](#design-details)
    - [Extension points](#extension-points)
      - [Filter](#filter)
      - [PreScore](#prescore)
      - [Score](#score)
  - [Known Limitations](#known-limitations)
  - [Alternatives considered](#alternatives-considered)
  - [Graduation Criteria](#graduation-criteria)
  - [Testing Plan](#testing-plan)
  - [Implementation History](#implementation-history)
  - [References](#references)
<!-- /toc -->

## Summary

This document describes filtering and scoring base on memory bandwidth and LLC (last level cache).

## Motivation

Memory bandwidth and LLC are very important resources in a system. Kuberenetes scheduler hasn't taken them into consideration yet. But memory bandwidth and LLC contention may greatly impact system performance. Moveover, it may cause poor SLA. For instance, in a CI/CD usage scenario, run 1 pipeline the totoal build time is 7min, run 16 pipelines the build time increases to 21min. Each pipeline (pod) is assigned the same CPU and memory, but the performance is quite different. We find once memory bandwidth is used out the performance will drop significantly, the more pipelines the worse performance. Intel® Resource Director Technology (Intel® RDT https://www.intel.com/content/www/us/en/architecture-and-technology/resource-director-technology.html) is one example of the technology that can be leveraged to mitigate the impact of memory bandwidth and LLC contention. It is very helpful to ensure system performance stability and improve SLA. 

## Goals
1. Use scheduler plugin, which is the most Kubernetes native way, to implement memory bandwidth and LLC aware filtering and scoring.
2. Leverage memory bandwidth and LLC relative metrics for filtering and scoring.
3. Provide configurable weights for prioritizing the metrics used in the scoring calculations.

## Non-Goals

## Use Cases
1. If a workload is memory bandwidth bond, e.g. rpmbuild, it uses much memory bandwidth. If too much such kind of workloads are scheduled to a node, the memory bandwith of the node will be used out quickly. The performance of the workloads will drop significantly. If the scheduler uses this filter plugin it can be avoid. 

2. If there are many workloads run on a node, the LLC contention may become very intense. The performance of all workloads will drop. If the scheduler uses this score plugin, it can ensure new workload is scheduled to LLC freer node. Furtherly, mitigate performance drop.

## Terms
- LLC: last level cache.
- RDT: Intel® Resource Director Technology.
- SLA: Service Level Agreement.
- MPKI: Misses Per Kilo Instructions.

## Proposal
We introduce some metrics such as memory bandwidth utilization, LLC occupation etc. At schedule stage, we can leverage these metrics to assist filtering and scoring. It is hard to determine how important a resource is and how much resource is required. We did off-line analysis to different scenario and get a series of coefficient of correlation and generic usage value. The coefficient is called affinity and the generic usage value is called profile. The affinities and profiles are provided by a yaml configuration file. Detail about how the affinity and profile are used is described in the following design details section. The average value of a period of time is more meaningful than immediate value for some metrics, for instance, LLC occupancy and CPU usage. Metrics data aren't obtained from agents (such as cAdvisor) directly, instead they are stored in Prometheus first and then are retrieved on demand.

## Design Details
The filtering and scoring plugin leverages the metrics retrieved from Prometheus to determine the node candidates for running a pod. The metrics considered by the plugin at the moment are:

* For filtering out oversubscribe nodes: 

    * free memory available (GB)
    * free memory bandwidth (GB/s)

* For scoring:

    * memory bandwidth (GB/s)
    * memory latency (ns)
    * Last Level Cache - l3 - Utilization (Bytes)
    * Last Level Cache contention -- (MPKI - misses per Kilo Instructions)
    * CPU utilization (%)

the detail about filtering and scoring will be described below. 

. Filtering

This aspect is resposible for filtering out any nodes that would not fit our pod's profile. 
The pod's profiles are created by analyzing top-down and finding an average scenario, which we can predict will happen on the cluster as well.

In principle, in order to avoid unnecessary oversubscription, we use an *OVERPROVISIONING* factor, by default set to 2. The factor is provided by the plugin configuration file.

Therefore the formulas we use are:

     if (TOTAL_MEMORY_BANDWIDTH - MEMORY_BANDWIDTH_UTILIZED > OVERPROVISIONING * MEMORY_BANDWIDTH_PROFILE ):
        return NODE_PASSED_MEMORY_BANDWITH_FILTER
     if (FREE_MEMORY > OVERPROVISIONING * MEMORY_UTILIZATION_PROFILE ):
        return NODE_PASSED_FREE_MEMORY_FILTER

Afterwards, a node that survives both filters passes the filtering.

. Scoring

For every pod each node will be assigned a list of scores based on:

* available resources when sorted by the scoring metrics (see above)
    * This means that for a pod, all the nodes will be sorted by memory bandwidth, memory latency, cpu, LLC contention and LLC utilization
    * All these lists will then go on to the next step


* the number of counterweights that are applicable to the node
    * A counterweight is a pod that has already been deployed on a node, but we assume it is still too soon to notice its impact in the measurements we receive from Prometheus. Therefore, we add its profile (memory bandwidth, cpu, and LLC utilization for now) as a counterweight for a *COUNTERWEIGTH_EXPIRATION_POLICY* period of time to the respective node

* ranking strategy when giving point to the sorted nodes
    * We allow flexible point awarding for our scheduler plugin. For example, our default strategy is to award 10 points to the node ranking first in a certain resource, 5 points to the second place, 1 point to the 3rd place and 0 points to anyone else.

* the affinity to the respective resource
    * The affinity is also obtained via a top-down analysis. It can have values between 0 (no impact) and 100 (very heavy impact) of a resource upon the pod execution time. The affinity of a resource acts a multiplier for the points awared in the ranking algorithm.
       Therefore, a node with a lot of available memory bandwith (ranking first) might not get a pod that needs less LLC thrashing in fact.

For pod that are not profiled yet, we allocate a default profile.

Therefore the overall score of a node is calculated by:

     # Assuming our scoring method for a 
     scoring_list = [10,5,1, [0]*n]
     WEIGHT_MEMORY_BANDWIDTH = 10
     WEIGHT_MEMORY_LATENCY = 3
     WEIGHT_LLC_OCCUPANCY = 6
     WEIGHT_LLC_MPKI= 2
     WEIGHT_CPU = 5

     node_score = scoring_list[position_of_node_ordered_by_memory_bandwidth] * WEIGHT_MEMORY_BANDWIDTH + 
                  scoring_list[position_of_node_ordered_by_memory_latency] *  WEIGHT_MEMORY_LATENCY +
                  scoring_list[position_of_node_ordered_by_llc_occupancy] *  WEIGHT_LLC_OCCUPANCY +
                  scoring_list[position_of_node_ordered_by_llc_mpki] *  WEIGHT_LLC_MPKI +
                  scoring_list[position_of_node_ordered_by_cpu] *  WEIGHT_CPU



### Extension points

#### Filter
When filter a node the memory and memory bandwidth requirement are estimated accroding to the classification of the pod to be scheduled. we check the free memory and free memory bandwidth of each node. In order to avoid unnecessary oversubscription, an OVERPROVISIONING factor is ued when calculate the memory and memory bandwidth to be reserved. This factor is got from the config file of this plugin, by default set to 2.

#### PreScore
At this extension point the name of the nodes to be scored are kept. At the Score extension point we will need rank these nodes to calculate the score.

#### Score

Calculate the score of a node according to the algorithm describe above. e.g. A workload is labeled as "incept-no-leak" (it has "app: incept-no-leak" label), according to the plugin configuration the affinity (that is to say the metric weights) of incept-no-leak type workload is: 
      memorybandwidth: 60
      memorylatency: 60
      llc_occupancy: 0
      llc_mpki: 0
      cpu: 50
Individual score is gived according to the postion. According to the plugin configuration the score gives to the top three nodes are 10, 5, 1, the score gives to other nodes are 0. Assume the postion of a node ordered by memory bandwidth, memory latency, LLC occupancy, LLC MPKI and CPU is 3， 2， 1， 4 and 2. The individual score of the node is 1, 5, 10, 0 and 5. The final score of the node is 1 * 60 + 5 * 60 + 10 * 0 + 0 * 0 + 5 * 50 = 610.

## Known Limitations

## Alternatives considered

## Graduation Criteria

## Testing Plan
1.  Add detailed unit and integration tests for workloads.
2.  Add basic e2e tests, to ensure all components are working together.

## Implementation History
## References

