# Preemption Toleration <!-- omit in toc -->

## Table of Contents <!-- omit in toc -->
<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Use Cases](#use-cases)
  - [Lower priority value but not-being-preempted priority](#lower-priority-value-but-not-being-preempted-priority)
  - [Guarantee to running at least N minutes even in lower priority](#guarantee-to-running-at-least-n-minutes-even-in-lower-priority)
- [Design Details](#design-details)
  - [Preemption Toleration API](#preemption-toleration-api)
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

- provide flexible preemption toleration policy spec
- and its implementation
 
### Non-Goals

- change Kubernetes public `PriorityClass` API

## Use Cases

### Lower priority value but not-being-preempted priority

Generally speaking, making a job be resumable after preemption, the job would need to preserve its state to outside of the pod, e.g. persistent volume, object storage, etc., at preemption and restore it at next restart (a.k.a check-pointing). However, when a job uses some proprietary tools that are not modifiable or do not support suspend/resume feature,  users would like to avoid being preempted. 

To achieve this, `PriorityClass` with a high priority value would be enough. But just giving high priority value causes preemption on any pods with lower priority values.  Sometimes, users don't want to do it.  In such a case, the job's priority is essentially low (i.e. the job is expected to run only when the cluster has a vacancy).  However, once scheduled, users don't want the pod to be preempted because the pod is difficult to export/import its state.

On the other hand, from the cluster administrative view, unconditional non-preempted pods are not desirable because there are system-critical, node-critical pods in the cluster. So, the cluster administrator would like to set the minimum priority value for such a priority class.  For example, assume there are three priority classes: system-critical, high and low.  Then, the cluster administrator would like to introduce `low-non-preempted` priority class that can not be preempted by high but can be preempted by system-critical.

```yaml
  system-critical: 10000
             high:  9000
              low:  8000
low-non-preempted:  8000 # with minimumPreemptablePriority=10000
                        # (i.e. high can't preempt this, but system-critical can preempt this)
```

### Guarantee to running at least N minutes even in lower priority

This is a typical use-case in machine learning job. As described above, job would need check-pointing. However, most machine learning jobs are iteration-based process, called _epochs_. So, if the cluster is high load and preemption always happens before the first epoch is finished,  the machine learning jobs can never make any progress and computational resources consumed by the job came to nothing in this case.

So, cluster administrators would like to exempt pods that do not finish the first epoch from preemption even if the job has lower priority. But, it is hard to monitor when the first epoch is finished outside of the job. Thus, a minimum running time is useful practically.

For example, cluster administrator would introduce several priority classes with the same priority value for introducing several minimum running time guarantee categories like below:

```yaml
    system-critical: 10000
                low:  8000  # no minimum running time guarantee
          low-10min:  8000  # 10 min minimum running time guarantee against 8000 < p < 10000
          low-30min:  8000  # 30 min minimum running time guarantee against 8000 < p < 10000
```

## Design Details

### Preemption Toleration API

Preemption toleration policy is defined in this struct type.

```go
// Please note that this does NOT have ObjectMeta 
// because this will be embedded in annotation in PriorityClass as unnamed object.
type PreemptionToleration struct {
  metav1.TypeMeta `json:",inline"`

  // MinimumPreemptablePriority specifies the minimum priority value that can preempt this priority class.
  MinimumPreemptablePriority *int32 `json:"minimumPreemptablePriority,omitempty"`

  // TolerationSeconds specified how many seconds this priority class can tolerate preemption by priorities lower than MinimumPreemptablePriority.  Null value specifies forever (default).  Duration means the duration from the pod being scheduled to some node.  This value affects only to scheduled pods (no effect on nominated nodes).
  TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`

  // more policy will arise in the future.
}
```

Users specifies the preemption toleration policy as an annotation with the key `scheduling.sigs.k8s.io/preemption-toleration` in `PriorityClass` as below:

```yaml
kind: PriorityClass
metadata:
  name: toleration-policy-sample
  annotation:
    scheduling.sigs.k8s.io/preemption-toleration: |
      # the api is versioned for future enhancement
      apiVersion: scheduling.sigs.k8s.io/v1beta1
      kind: PreemptionToleration

      # This priority class can tolerate preemption by priority with 8000 <= p < 10000.
      minimumPreemptablePriority: 10000

      # And it can tolerate preemption in 1 hour by the pod with priority (8000 <= p < 10000).
      tolerationSeconds: 3600
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
	ph framework.PreemptHandle,
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
