# Memory bandwidth and LLC aware filtering and scoring

## Table of Contents

<!-- toc -->
- [RDT aware filtering and scoring](#RDT-aware-filtering-and-scoring)
  - [Table of Contents](#table-of-contents)
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

## Motivation

Memory bandwidth and LLC (last level cache) are very important resources in a system. Kuberenetes scheduler hasn't taken them into consideration yet. But memory bandwidth and LLC contention may impact system performance very much. Furtherly, it causes bad SLA. Intel Resource Director Technology (RDT) is one example of the technology that can be leveraged to mitigate the impact of memory bandwidth and LLC contention. It is very helpful to ensure system performance stability and improve SLA. 

## Goals
1. Use scheduler plugin, which is the most Kubernetes native way, to implement memory bandwidth and LLC aware filtering and scoring.
2. Leverage memory bandwidth and LLC relative metrics for filtering and scoring.
3. Provide configurable weigths for prioritizing the metrics used in the scoring calculations.

## Non-Goals

## Use Cases

## Terms
LLC: last level cache.
RDT: Intel Resource Director Technology.
SLA: Service Level Agreement.
MPKI: Misses Per Kilo Instructions.

## Proposal
We introduce some metrics such as memory bandwidth utilization, LLC occupation etc. At schedule stage, we can leverage these metrics to assist filting and scoring. It is hard to determine how important a resource is and how much resource is required. We did off-line analysis to different scenario and get a series of coefficient of correlation and generic usage value. The coefficient is called affinity and the generic usage value is called profile. The affinities and profiles are provided by a yaml configuration file. Detail about how the affinity and profile are used is described in the following design details section. The average value of a period of time is more meaningful than immediate value for some metrics, for instance, LLC occupancy and CPU usage. Metrics data aren't obtained from agents (such as cAdvisor) directly, instead they are stored in Prometheus first and then are retrieved on demand.

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

     if (TOTAL_MEMORY_BANDWIDTH - MEMORY_BANDWIDTH_UTILIZED > OVERPROVISIONING * FUNCTION_MEMORY_BANDWIDTH_PROFILE ):
        return NODE_PASSED_MEMORY_BANDWITH_FILTER
     if (FREE_MEMORY > OVERPROVISIONING * FUNCTION_MEMORY_UTILIZATION_PROFILE ):
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

* the pod affinity to the respective resource
    * The pod affinity is also obtained via a top-down analysis. It can have values between 0 (no impact) and 100 (very heavy impact) of a resource upon the pod execution time. The affinity of a pod acts a multiplier for the points awared in the ranking algorithm.
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
In kubernetes scheduler v2 framework nodes are scored in parallel, to improve efficiency, and associate with the feature of our algorithm, we calculate the score of the nodes that have passed the filtering phase at the prescore extension point.

#### Score
As have calculated the score at the PreScore extension point. just simply return the score directly at this point.

## Known Limitations

## Alternatives considered

## Graduation Criteria

## Testing Plan
1.  Add detailed unit and integration tests for workloads.
2.  Add basic e2e tests, to ensure all components are working together.

## Implementation History
## References

