# Deploying the NodeResourceTopology plugins

This document contains notes and outrline the challenges related to run the NodeResourceTopology plugin.
The intent is to share awareness about the challenges and how to overcome them.
**The content here applies to plugins running with the overreserve cache enabled.**

Last updated: December 2023 - last released version: 0.27.8

## As a secondary scheduler

Running the NodeResourceTopology plugin as part of the secondary scheduler is currently the recommended approach.
This way only the workload which wants stricter NUMA alignment can opt-in, while any other workload, including all
the infrastructure pods, will keep running as usual using the battle-tested main kubernetes scheduler.

### Interference with the main scheduler

For reasons explained in detail in the
[cache design docs](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/pkg/noderesourcetopology/cache/docs/NUMA-aware-scheduler-side-reserve-plugin.md#the-reconciliation-condition-a-node-state-definition)
the NRT plugin needs to constantly compute the expected nodes state in terms of which pods are running there.
Is not possible (nor actually desirable) to completely isolate worker nodes while the NRT code runs in the secondary
scheduler. The main scheduler is always allowed to schedule pods on all the nodes, if nothing else to manage infra
pods (pods needed by k8s itself to work). It is theoretically possible to achieve this form of isolation, but
the setup would be complex and likely fragile, so this option is never fully explored.

When both the main scheduler and the secondary scheduler bind pods to nodes, the latter needs to deal with the fact
that pods can be scheduled outside its own control; thus, the expected computed state need to be robust against
this form of interference. The logic to deal with this interference is implemented in the
[foreign pods][1] [management](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/pkg/noderesourcetopology/cache/foreign_pods.go).

While NRT runs as secondary scheduler, this form of interference is practically unavoidable.
The effect of the interference would be slower scheduling cycle, because the NRT plugin will need to compute the
expected node state more often and to possibly invalidate partial computations, beginning again from scratch,
which will take more time.

In order to minimize the recomputation without compromising accuracy, we introduced a cache tuning option
[ForeignPodsDetect](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/apis/config/v1/types.go#L179).
Currently, the foreign pods detection algorithm is triggered by default when any foreign pod is detected.
Testing is in progress to move the default to trigger when pods which require exclusive resources are
requested, because only these exclusive resources can have NUMA locality, then contribute to the accounting done by the plugin.

### Efficient computation of the expected node states

In order to compute the expected node state, by default the plugin considers all the pods running on a node, regardless
of their resource requests. This is wasteful and arguably incorrect (albeit not harmful) because only exclusive resources
can possibly have NUMA affinity. So containers which don't ask for exclusive resources don't contribute to per-NUMA resource
accounting, and can be safely ignored.

Minimizing the set of pods considered for the state reduces the churn and make the state change less often.
This decreases the scheduler load and gives better performance because minizes the downtime on which scheduling waits
for recomputation. Thus, we introduced a cache tuning option
[ResyncMethod](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/apis/config/v1/types.go#L186).
Testing is in progress to switch the default and only compute the state using containers requiring exclusive resources.

## As part of the main scheduler

The NodeResourceTopology code is meant for eventual merge into core kubernetes.
The process is still ongoing and requires some dependenciues to be sorted out before.

### Patching the main scheduler

Until the NodeResourceTopology code (and the noderesourcetopology API) becomes part of the core k8s codebase, users
willing to run just one scheduler needs to patch their codebase and deploy and updated binary.
While we don't support this flow yet, we're also *not* aware of any specific limitation preventing it.

### Handling partial NRT data

A common reason to want to use just one scheduler is to minimize the moving parts, and perhaps even stronger, to
avoid partitioning the worker node set. In this case, NRT data may be available only by a subset of nodes, which
are the ones meant to run the NUMA-alignment-requiring workload.

By design, the NodeResourceTopology Filter plugin
[allows nodes which don't have NRT data attached](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/pkg/noderesourcetopology/filter.go#L212).
This is a prerequisite to make the NodeResourceTopology plugin work at all in cases like this, with partial NRT
availability.

The NodeResourceTopology score plugin is designed to
[always prefer nodes with NRT data available](https://github.com/kubernetes-sigs/scheduler-plugins/pull/685),
everything else equal.

The combined effect of the Filter and Score design is meant to make the NodeResourceTopology work in this scenario.
However, up until now the recommendation was to partition the worker nodes if possible, so this scenario is less tested.

The reader is advised to test this scenario carefully, while we keep promoting it and add more tests to ensure adequate coverage.

### Efficient computation of the expected node states

Same considerations applies like for the case of running as secondary schedulers, because computing the expected node
state is a pillar of the `overreserve` cache functionality.

--- 

[1]: A pod is "foreign" when it is known by the scheduler only _ex-poste_, after it was scheduled; so the scheduling decision was
     made by a different scheduler and this instance needs to deal with that.
