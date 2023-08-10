# Preemption Toleration <!-- omit in toc -->

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Use Cases](#use-cases)
  - [Lower priority value but not-being-preempted priority](#lower-priority-value-but-not-being-preempted-priority)
  - [Conditional preemption with guaranteed running time](#conditional-preemption-with-guaranteed-running-time)
- [Design Details](#design-details)
  - [Preemption Toleration API](#preemption-toleration-api)
  - [Implementing Typical Use Cases by Preemption Toleration APIs](#implementing-typical-use-cases-by-preemption-toleration-apis)
    - [Lower priority value but not-being-preempted priority](#lower-priority-value-but-not-being-preempted-priority-1)
    - [Conditional preemption with guaranteed running time](#conditional-preemption-with-guaranteed-running-time-1)
  - [Plugin implementation](#plugin-implementation)
    - [PostFilter](#postfilter)
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

This plugin provides extended behavior of [Priority and Preemption](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/) in Kubernetes Scheduler.

Especially, this plugin enables cluster administrators to define preemption policy from the _victim_ side which is not covered by public `PriorityClass` API. This plugin provides _preemption toleration_ policy in `PriorityClass`, which defines the criteria by which the priority class will be exempt from preemption.

## Motivation

Kubernetes scheduler provides [Pod Priority and Preemption](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/) feature.  Users, typically cluster administrators, can define several `PriorityClass` to classify priority in the cluster.  If the cluster is less elastic (e.g. On-Premise clusters), designing priority classes and preemption rules are very important for computational resource utilization.

`PriorityClass` can have `PreemptionPolicy` configuration for customizing preemptor behavior for the priority class. The allowed values are `PreemptLowerPriority`(default) and `Never`.  If `Never` is set, the priority class becomes [_Non-preempting priority class_](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/#non-preempting-priority-class) that can not preempt any pods but may be preempted by higher priority pods.  The `PreemptionPolicy` API is very simple and understandable.  However, this policy only focuses on preemptor side behavior.

This plugin provides more flexible preemption behavior configuration by adding the preemptee (a.k.a victim) side policy in `PriorityClass`. In particular, cluster administrators can define _preemption toleration_ policy that defines the criteria which the priority class will be exempt from preemption.

### Goals

- provide flexible preemption toleration policy API spec and its implementation
 
### Non-Goals

- change Kubernetes public `PriorityClass` API

## Use Cases

### Lower priority value but not-being-preempted priority

Generally speaking, making a job be resumable after preemption, the job would need to preserve its state externally, e.g. persistent volume, object storage, etc., at preemption and restore it at next restart (a.k.a check-pointing). However, when a job uses some proprietary tools that are not modifiable or do not support suspend/resume feature,  users would like to avoid being preempted. 

To achieve this, `PriorityClass` with a high priority value would be enough. But just giving high priority value causes preemption on any pods with lower priority values.  Sometimes, users don't want to do it.  In such a case, the job's priority is essentially low (i.e. the job is expected to run only when the cluster has a vacancy).  However, once scheduled, users don't want the pod to be preempted because the pod is difficult to export/import its state.

On the other hand, from the cluster administrative view, unconditional non-preempted pods are not desirable because there are system-critical, node-critical pods in the cluster. So, the cluster administrator would like to set the minimum priority value for such a priority class.  For example, assume there are three priority classes: system-critical, high and low.  Then, the cluster administrator would like to introduce `low-non-preempted` priority class that can not be preempted by high but can be preempted by system-critical.

```yaml
  system-critical: 10000
             high:  9000
low-non-preempted:  8000 # this priority class can tolerate preemption from priorities less than 10000
                         # (i.e. high can't preempt this, but system-critical can preempt this)
              low:  8000 # normal preemption happens on this priority class, i.e. p > 8000 can preempt this priority class.
```

To realise this feature, the plugin introduces `MinimumPreemptablePriority` in the preemption toleration policy API. See [Preemption Toleration API](#preemption-toleration-api) section and [Implementing Typical Use Cases by Preemption Toleration APIs](#implementing-typical-use-cases-by-preemption-toleration-apis) section below.

### Conditional preemption with guaranteed running time 

This is a typical use-case in machine learning job. As described above, job would need check-pointing. However, most machine learning jobs are iteration-based process, called _epochs_. So, if the cluster is high load and preemption always happens before the first epoch is finished,  the machine learning jobs can never make any progress and computational resources consumed by the job came to nothing in this case.

So, cluster administrators would like to exempt pods that do not finish the first epoch from preemption even if the job has lower priority. But, it is hard to monitor when the first epoch is finished outside of the job. Thus, a minimum running time is useful practically.

For example, cluster administrator would introduce several priority classes with the same priority value for introducing several minimum running time guarantee categories like below. 

```yaml
        system-critical: 10000  # system-critical can preempt pods in low-non-preempted-{10,30}min priority immediately
                   high:  9000  # high can preempt pods in low-non-preempted-{10,30}min priority class which elapsed at least {10,30} minutes.
                    low:  8000  # no minimum running time guarantee
low-non-preempted-10min:  8000  # 10 min minimum running time guarantee against 8000 < p < 10000
low-non-preempted-30min:  8000  # 30 min minimum running time guarantee against 8000 < p < 10000
```

To realise this feature, the plugin also introduces `TolerationSeconds` in the preemption toleration policy API. See [Preemption Toleration API](#preemption-toleration-api) section and [Implementing Typical Use Cases by Preemption Toleration APIs](#implementing-typical-use-cases-by-preemption-toleration-apis) section below.

## Design Details

### Preemption Toleration API

Preemption toleration policy is defined in this struct type.

```go
type PreemptionToleration struct {
  // MinimumPreemptablePriority specifies the minimum priority value that can preempt this priority class.
  // It defaults to the PriorityClass's priority value + 1 if not set, which means pods that have a higher priority value can preempt it.
  MinimumPreemptablePriority *int32 `json:"minimumPreemptablePriority,omitempty"`

  // TolerationSeconds specifies how long this priority class can tolerate preemption 
  // by priorities lower than MinimumPreemptablePriority.
  // It defaults to zero if not set. Zero value means the pod will be preempted immediately. i.e., no toleration at all. 
  // If it's set to a positive value, the duration will be honored.
  // If it's set to a negative value, the pod can be tolerated forever - i.e., pods with priority
  // lower than MinimumPreemptablePriority won't be able to preempt it.
  // This value affects scheduled pods only (no effect on nominated pods).
  TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`

  // more policy will arise in the future.
}
```

Users specifies the preemption toleration policy as an annotation with the key `preemption-toleration.scheduling.x-k8s.io/<property_name>` in `PriorityClass` as below:

```yaml
kind: PriorityClass
metadata:
  name: toleration-policy-sample
  annotation:
    preemption-toleration.scheduling.x-k8s.io/minimum-preemptable-priority: "10000"
    preemption-toleration.scheduling.x-k8s.io/toleration-seconds: "3600"
value: 8000
```


### Implementing Typical Use Cases by Preemption Toleration APIs

This section describes how to implement scenarios described in [Use Cases](#use-cases) section by the preemption toleration policy.

#### Lower priority value but not-being-preempted priority

Assume cluster administrator introduces `low-non-preempted` priority class that can not be preempted by high but can be preempted by system-critical.  Then, they would declare `low-non-preempted` `PriorityClass` with preemption toleration policy of `MinimumPreemptablePriority=10000,TolerationSeconds=-1` like below:

```yaml
# system-critical can preempt low-10min pods immediately 
# because low-non-preempted-10min can't tolerate the preemption from this priority class
# (because of MinimumPreemptablePriority=10000)
  system-critical: 10000

# high pods can not preempt low-non-preempted pods forever
# because low-non-preempted priority can tolerate the preemption forever 
# by priority class p < 10000(=MinimumPreemptablePriority)
# (because of MinimumPreemptablePriority=10000 and TolerationSeconds=-1)
             high:  9000

# with MinimumPreemptablePriority=10000,TolerationSeconds=-1
low-non-preempted:  8000 

# low with no preemption toleration policy. normal preemption behavior,
# i.e. this priority class will be preempted by priority p > 8000
              low:  8000
```

Thus, `low-non-preempted` manifest would be like below:

```yaml
kind: PriorityClass
metadata:
  name: low-non-preempted
  annotation:
    # This priority class can tolerate preemption by priority with p < 10000.
    preemption-toleration.scheduling.x-k8s.io/minimum-preemptable-priority: "10000"
    # This priority class can tolerate preemption forever by priority with p < 10000(=minimum-preemptable-priority)
    preemption-toleration.scheduling.x-k8s.io/toleration-seconds: "-1"
value: 8000
```

#### Conditional preemption with guaranteed running time

Assume cluster administrator introduces `low-non-preempted-10min` priority class that can tolerate the preemption by high priority class for at lease 10 minutes, i.e. this guarantees 10 minutes running time for the priority class.  Then, they would declare `low-non-preempted-10min` `PriorityClass` with preemption toleration policy of `MinimumPreemptablePriority=10000,TolerationSeconds=600` like below:

```yaml
# system-critical can preempt low-non-preempted-10min pods immediately 
# because low-non-preempted-10min can't tolerate the preemption from this priority class
# (because of MinimumPreemptablePriority=10000)
         system-critical: 10000 

# high pods can preempt low-non-preempted-10min pods which elapsed 
# at least 10min from being scheduled because toleration expired in 10 minutes
# (because of MinimumPreemptablePriority=10000 and TolerationSeconds=600)
                   high:  9000 

# with MinimumPreemptablePriority=10000, TolerationSeconds=600
low-non-preempted-10min:  8000
```

Thus, `low-non-preempted-10min` manifest would be like below:

```yaml
kind: PriorityClass
metadata:
  name: low-non-preempted-10min
  annotation:
    # This priority class can tolerate preemption by priority with p < 10000.
    preemption-toleration.scheduling.x-k8s.io/minimum-preemptable-priority: "10000"
    # This priority class can tolerate preemption for 10 minutes (600 seconds) 
    # by priority with p < 10000(=minimum-preemptable-priority)
    preemption-toleration.scheduling.x-k8s.io/toleration-seconds: "600"
value: 8000
```

### Plugin implementation

#### PostFilter

This plugin implements the PostFilter extension point because this plugin focuses to extend the scheduler's preemption behavior. Moreover, this plugin just extends victim candidate selection logics in the default preemption behavior. 

The modification would be only in `selectVictimsOnNode` as below:

```golang
// selectVictimsOnNode finds a minimal set of pods on the given node that should
// be preempted in order to make enough room for "preemptor" to be scheduled.
// The algorithm is almost identical to DefaultPreemption plugin's one.
// The only difference is that it takes PreemptionToleration annotations in
// PriorityClass resources into account for selecting victim pods.
func (pl *PreemptionToleration) selectVictimsOnNode(
	ctx context.Context,
	ph framework.Handle,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget,
	now time.Time,
) ([]*v1.Pod, int, bool) {
	...
	// As the first step, remove all lower priority pods 
	// that can NOT tolerate preemption by the preemptor 
	// from the node.
	podPriority := corev1helpers.PodPriority(pod)
	for _, p := range nodeInfo.Pods {
		canToleratePreemption, err := CanToleratePreemption(p.Pod, pod, pl.priorityClassLister, now)
		if err != nil {
			klog.Warningf("Encountered error while selecting victims on node %v: %v", nodeInfo.Node().Name, err)
			return nil, 0, false
		}
		if corev1helpers.PodPriority(p.Pod) < podPriority && !canToleratePreemption {
			potentialVictims = append(potentialVictims, p.Pod)
			if err := removePod(p.Pod); err != nil {
				return nil, 0, false
			}
		}
	}

	// In tha later steps, it calculates a 'minimal' victim pods set 
	// by reprieving removed pods above and check preemptor can fit 
	// the node in one-by-one manner
	...
```

`CanToleratePreemption` is the function which evaluate `PreemptionToleration` policy and returns it can tolerate.  The code would be like this:

```golang
// CanToleratePreemption evaluates whether the victimCandidate pod 
// can tolerate from preemption by the preemptor pod or not
// by inspecting PriorityClass of victimCandidate pod.
// NOTE: The function will be public so that other plugins can evaluate PreemptionToleration policy
func CanToleratePreemption(
	victimCandidate, preemptor *v1.Pod,
	priorityClassLister schedulingv1listers.PriorityClassLister,
	now time.Time,
) (bool, error) {
	preemptorPriority := corev1helpers.PodPriority(preemptor)

	if victimCandidate.Spec.PriorityClassName == "" {
		return false, nil
	}

	victimCandidatePriorityClass, err := priorityClassLister.Get(victimCandidate.Spec.PriorityClassName)
	if err != nil {
		return false, err
	}

	// get completed PreemptionToleration policy (it completes default values)
	toleration, err := getCompletingPreemptionToleration(*victimCandidatePriorityClass)
	if err != nil || toleration == nil{
		return false, err
	}

	// check it can tolerate the preemption in terms of priority value
	canTolerateOnPriorityValue := preemptorPriority < *toleration.MinimumPreemptablePriority
	if !canTolerateOnPriorityValue {
		return canTolerateOnPriorityValue, nil
	}

	if toleration.TolerationSeconds == nil {
		return canTolerateOnPriorityValue, nil
	}

	// check it can tolerate the preemption in terms of toleration seconds
	_, scheduledCondition := podutil.GetPodCondition(&victimCandidate.Status, v1.PodScheduled)
	if scheduledCondition == nil {
		return canTolerateOnPriorityValue, nil
	}
	canTolerateOnTolerationSeconds := !now.After(
		scheduledCondition.LastTransitionTime.Time.Add(time.Duration(*toleration.TolerationSeconds)*time.Second),
	)

	return canTolerateOnTolerationSeconds, nil
}
```

## Implementation History

- 2021-06-24: Initial KEP sent out for review, including Summary, Motivation, etc.
