# Capacity scheduling 

## Table of Contents

<!-- toc -->
- [Release Signoff Checklist](#release-signoff-checklist)
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Relationship with ResourceQuota](#relationship-with-resourcequota)
  - [User Stories (optional)](#user-stories-optional)
    - [Story 1](#story-1)
    - [Story 2](#story-2)
- [Design Details](#design-details)
  - [Extention point](#extention-point)
    - [PreFilter](#prefilter)
    - [PostFilter](#postfilter)
    - [Cache](#cache)
  - [Additional Preemption Details](#additional-preemption-details)
    - [⚠️ Cross-namespace vs. single-namespace preemption](#-cross-namespace-vs-single-namespace-preemption)
      - [Elastic Quota Configuration for namespace 1](#elastic-quota-configuration-for-namespace-1)
      - [Elastic Quota Configuration for namespace 2](#elastic-quota-configuration-for-namespace-2)
      - [Elastic Quota Configuration for namespace 3](#elastic-quota-configuration-for-namespace-3)
      - [Deployment on Namespace quota1](#deployment-on-namespace-quota1)
      - [A sample Priority Class Deployment](#a-sample-priority-class-deployment)
      - [Deployment on Namespace quota2](#deployment-on-namespace-quota2)
  - [Known Limitations](#known-limitations)
    - [⚠️ Cross Node Preemption is not supported](#-cross-node-preemption-is-not-supported)
      - [Elastic Quota Configuration for namespace 1](#elastic-quota-configuration-for-namespace-1-1)
      - [Elastic Quota Configuration for namespace 2](#elastic-quota-configuration-for-namespace-2-1)
      - [Run Sample Deployment on namespace 1](#run-sample-deployment-on-namespace-1)
      - [Run Sample Deployment on namespace 2](#run-sample-deployment-on-namespace-2)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
  - [Upgrade / Downgrade Strategy](#upgrade--downgrade-strategy)
  - [Version Skew Strategy](#version-skew-strategy)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Release Signoff Checklist

Items marked with (R) are required *prior to targeting to a milestone / release*.

- [ ] (R) Enhancement issue in release milestone, which links to KEP dir in [kubernetes/enhancements] (not the initial KEP PR)
- [ ] (R) KEP approvers have approved the KEP status as `implementable`
- [ ] (R) Design details are appropriately documented
- [ ] (R) Test plan is in place, giving consideration to SIG Architecture and SIG Testing input
- [ ] (R) Graduation criteria is in place
- [ ] (R) Production readiness review completed
- [ ] Production readiness review approved
- [ ] "Implementation History" section is up-to-date for milestone
- [ ] User-facing documentation has been created in [kubernetes/website], for publication to [kubernetes.io]
- [ ] Supporting documentation e.g., additional design documents, links to mailing list discussions/SIG meetings, relevant PRs/issues, release notes

<!--
**Note:** This checklist is iterative and should be reviewed and updated every time this enhancement is being considered for a milestone.
-->

[kubernetes.io]: https://kubernetes.io/
[kubernetes/enhancements]: https://git.k8s.io/enhancements
[kubernetes/kubernetes]: https://git.k8s.io/kubernetes
[kubernetes/website]: https://git.k8s.io/website

## Summary

<!--
This section is incredibly important for producing high quality user-focused
documentation such as release notes or a development roadmap.  It should be
possible to collect this information before implementation begins in order to
avoid requiring implementors to split their attention between writing release
notes and implementing the feature itself.  KEP editors, SIG Docs, and SIG PM
should help to ensure that the tone and content of the `Summary` section is
useful for a wide audience.

A good summary is probably at least a paragraph in length.

Both in this section and below, follow the guidelines of the [documentation
style guide]. In particular, wrap lines to a reasonable length, to make it
easier for reviewers to cite specific portions, and to minimize diff churn on
updates.

[documentation style guide]: https://github.com/kubernetes/community/blob/master/contributors/guide/style-guide.md
-->

## Motivation

There is increasing demand to use Kubernetes to manage batch workloads 
(ML/DL). In those cases, one challenge is to improve cluster utilization 
while ensuring that each user has a reasonable amount of resources. The 
problem can be partially addressed by the Kubernetes [ResourceQuota].
The native Kubernetes ResourceQuota API can be used to specify the maximum 
overall resource allocation per namespace. The quota enforcement is done 
through an admission check. A quota consumer (e.g., a Pod) cannot be 
created if the aggregated resource allocation exceeds the quota limit. 
In other words, the overall resource usage is aggregated based on Pod's
spec (i.e., cpu/mem requests) when it's created. The Kubernetes quota design 
has the limitation: the quota resource usage is aggregated based on the 
resource configurations (e.g., Pod cpu/mem requests specified in the Pod 
spec). Although this mechanism can guarantee that the actual resource 
consumption will never exceed the ResourceQuota limit, it might lead to low 
resource utilization as some pods may have claimed the resources but failed 
to be scheduled. For instance, actual resource consumption may be much smaller 
than the limit.

Due to above limitation, the batch workloads (ML/DL) can't run in a 
Kubernetes cluster as efficiently as they do in other container orchestration
platforms such as Yarn. In order to overcome above limitations, an 
"ElasticQuota" concept used in [Yarn capacity scheduler] can be 
leveraged into Kubernetes. Basically, the "ElasticQuota" has the notions of 
"max" and "min".

- max: the upper bound of the resource consumption of the consumers.

- min: the minimum resources that are guaranteed to ensure the basic  
functionality/performance of the consumers

We proposed a new "ResourceQuota" mechanics to optimize current capacity 
scheduling as follows:

1. The resource buffer between "min" and "max" can help to tolerate runtime 
failures. For example, if a Pod that consumes the entire "min" fails to run, 
new Pods can still be created if the "max" has not been reached. When using 
Kubernetes resource quota, once the Pod that consumes the entire quota is 
created and the namespace reaches its quota limit, no other Pods can be 
created - even if the Pod failed to be scheduled, or failed on running (error
 when pulling image, etc.).
2. Allow the workloads from one quota to "borrow" unused reserved "min" 
resources from other quotas. A quota's unused "min" resource can be used by 
other users, under the condition that there is a mechanism to guarantee the 
"victim" user can consume its "min" resource whenever it needs.

The success of "ElasticQuota" based capacity scheduling relies on a proper 
Pod preemption algorithm implementation. This KEP proposes the minimal 
scheduler extension to support the "ElasticQuota" based scheduling based on 
the scheduler framework.

[ResourceQuota]: https://kubernetes.io/docs/concepts/policy/resource-quotas/
[Yarn capacity scheduler]: https://www.netjstech.com/2018/04/capacity-scheduler-in-yarn-hadoop.html

### Goals

- The API definition of ElasticQuota
- Implement a scheduler plugin to honor the API of elastic resource quota.

### Non-Goals

<!--
What is out of scope for this KEP?  Listing non-goals helps to focus discussion
and make progress.
-->

## Proposal

A "CRD" is needed to interact with the scheduler. The CRD kind name is 
subject to change. We use "ElasticQuota" in the proposal. The CRD defines the
min and max allocation for the managed resources such as cpu, memory or 
extended resources. "ElasticQuota" is namespace scoped. 

```go
// ElasticQuota sets elastic quota restrictions per namespace
type ElasticQuota struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	Spec   ElasticQuotaSpec
	Status ElasticQuotaStatus
}

// ElasticQuotaSpec defines the Min and Max for Quota.
type ElasticQuotaSpec struct {
	Min v1.ResourceList
	Max v1.ResourceList
}

// ElasticQuotaStatus defines the observed use.
type ElasticQuotaStatus struct {
	Used v1.ResourceList
}
```

sample yaml is listed below:

```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: test
  namespace: test
spec:
  max:
    cpu: 20
    memory: 40Gi
    nvidia.com/gpu: 2
  min:
    cpu: 10
    memory: 20Gi
    nvidia.com/gpu: 1
```

The definitions of "min" and "max" have been explained above. Max is infinite
(in particular `math.MaxInt64`) by default, min is 0 by default.
The sum of min in all ElasticQuota must be 
less than or equal to the total resource capacity of the cluster.The 
ElasticQuota objects are created by a cluster administrator. One ElasticQuota
object per namespace.

### Relationship with ResourceQuota
This version of proposal does not discuss merging the ElasticQuota into the 
existing ResourceQuota. But in the future, we will try to merge ElasticQuota 
into ResourceQuota when it's mature. We have three fields of resource in 
ElasticQuota(min max) and ResourceQuota(hard). They act on different phases 
of pod lifecycle. So it maybe possible to merge them together.
- hard: it will check the resource in pod creation through an admission check
. It will prevent submitting too many jobs at one time. The usage of hard is 
aggregated based on the resource configurations of all pods so it will be 
greater than or equal to max. 

- max: it will check the resource in the Prefilter of Pod Scheduling Cycle. 
The usage of max is aggregated based on the resource configurations of 
successfully scheduled pods.

- min: it means the guaranteed resource of Quota.

min and max need to respect the namespace's ResourceQuota limit, i.e., `min 
<= max <= hard` . 


### User Stories (optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system.  The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1
We assume two elastic quotas are defined: QuotaA (min:4, max:6) and QuotaB 
(min:6, max:8), the quota unit is the number of GPUs. UserA in green color 
consumes QuotaA and UserB in red color consumes QuotaB. The entire cluster 
has 10 GPUs available hence the sum of quota min is equal to the cluster capacity.

Initially, UserA consumes 2 GPUs and UserB consumes 3 GPUs. Both consumptions
are within the reserved "min" quota range.

![1](./1.png)


Next, UserA consumes 4 GPUs reaching the "min" quota of QuotaA. UserB still 
consumes 3 GPUs. The cluster still has sufficient GPU resources hence no 
resource stealing happens.

![2](./2.png)

Assuming UserA attempts to use the rest of 3 unused GPUs in the cluster,
only 2 more GPUs can be allocated to reach its quota "max", which is 6 GPUs.
Note that in this case, 2 GPUs are "borrowed" from UserB's "Guaranteed" quota
since UserB is not using them at that moment. 

![3](./3.png)

Later, if UserB demands 3 more GPUs compared to its current consumption, 
its request cannot be immediately satisfied since only one unused GPU left in
the cluster. Since UserB's "min" quota is 6 GPUs, the preemption algorithm 
ensures that three GPUs will be returned back by preempting a pod which 
requested 2 GPUs from UserA, i.e., from 6/3 to 4/6. Then if UserB requests 1 
or 2 more GPUs, it won't succeed as GPUs in UserA by that moment are all 
guaranteed and hence non-preemptable. 

![4](./4.png)

In the end, UserA consumes 4 GPUs and UserB consumes 6 GPUs, satisfying each 
user's "min" quota.

#### Story 2
We assume three elastic quotas are defined: QuotaA (min:3,  max:4), QuotaB
(min:4, max:6) and QuotaC (min:3,  max:4), the quota unit is the number of GPUs. UserA in blue color
consumes QuotaA, UserB in red color consumes QuotaB and UserC in green color consumes QuotaC. The entire cluster
has 10 GPUs available hence the sum of quota min is equal to the cluster capacity.

Initially, UserA consumes 2 GPUs, UserB consumes 2 GPUs and UserC consumes 3 GPUS. All consumptions
are within the reserved "min" quota range.

![5](./5.png)

Next, UserA consumes 4 GPUs reaching the "max" quota of QuotaA.UserB consumes 3 GPUs. UserC still consumes 3 GPUs.

![6](./6.png)

Later, UserB submits a pod that requests 1 GPU, at this time the cluster does not have enough resources and needs 
to trigger a preemption. When `QuotaB.used + Preemptor.request <= QuotaB.min`, victims will be selected from the EQ which used > min
In this case, `QuotaA.used > min` and `QuotaC.used <= min`. So a pod in QuotaA will be the victim.

![7](./7.png)

At some point, UserA consumes 2GPUs and UserC consumes 3 GPUs. UserB's two pods consume a total of 5 GPUs.

![8](./8.png)

Next, UserB submits a PodC that requests 1 GPU, at this time the cluster does not have enough resources and needs
to trigger a preemption. When `QuotaB.used + Preemptor.request > QuotaB.min`, victims will be selected from the same Quota. So PodA will be the victim.

![9](./9.png)


## Design Details

### Extention point

#### PreFilter
We will check if the (Pod.request + Quota.Allocated) is less than Quota.Max. 
If not, the scheduling cycle of PodA will fail. 

#### PostFilter
We will reimplement the method `selectVictimsOnNode` in `defaultPreempt`. The
original `selectVictimsOnNode` method selects all the pods with the lower
priority than the preemptor’s priority as potential victims in a node. 

In this proposal, the potential victims selection logic needs to be changed 
in the following cases.

- if Preemptor.Request + Quota.allocated <= Quota.min: It means that its min 
or guaranteed resource is used or `borrowed` by other Quota. Potential 
victims in a node will be chosen from Quotas that allocates more resources 
than its min, i.e., borrowing resources from other Quotas.
- if Preemptor.Request + Quota.allocated > Quota.min: It means that its 
guaranteed isn't borrowed by other quotas. So that we will select the pods 
belongs to the same quota(namespace) with the lower priority than the 
preemptor’s priority as potential victims in a node.

#### Cache
We will watch the event of ElasticQuota and pod. The status of ElasticQuota 
like allocated will store in Cache.

### Additional Preemption Details
Preemption happens when a pod is unschedulable, i.e., failed in PreFilter or Filter phases.

In particular for capacity scheduling, the failure reasons could be:
- Prefilter Stage
  - sum(allocated res of pods in the same elasticquota) + pod.request > elasticquota.spec.max
  - sum(allocated res of pods in the same elasticquota) + pod.request > sum(elasticquota.spec.min)

So the preemption logic will attempt to make the pod schedulable, with a cost of preempting other running pods.


#### ⚠️ Cross-namespace vs. single-namespace preemption
During the preemption, if allowing the current pod leads to excessive usage of its elastic quota's min, the plugin will only try to preempt lower-priority pods within this preemptor pod's namespace. That's to say, it won't preempt the lower-priority pods among other namespaces even if they have overused their elastic quota's min.

This is to adhere to the elastic quota's API semantics:

- wild cross-namespace preemption to guarantee an elastic quota's min resource
- restricted single-namespace preemption if the aggregated usage is larger than min

Below is a simple example. 
##### Elastic Quota Configuration for namespace 1
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota1
  namespace: quota1
spec:
  max:
    cpu: 2
  min:
    cpu: 0
```
##### Elastic Quota Configuration for namespace 2
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota2
  namespace: quota2
spec:
  max:
    cpu: 2
  min:
    cpu: 0
```
##### Elastic Quota Configuration for namespace 3
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota3
  namespace: quota3
spec:
  max:
    cpu: 2
  min:
    cpu: 1
```
##### Deployment on Namespace quota1
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota1
  labels:
    app: nginx
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      schedulerName: capacityscheduler
      containers:
        - name: nginx
          image: nginx
          resources:
            limits:
              cpu: 1
            requests:
              cpu: 1
```
The nginx pod is able to run because it will borrow the 1 free cpu min from namespace quota3. Note, by default if the PriorityClassName is not configured, the pod will have a priority of 0.

Next, let's try to run another deployment with higher priority on namespace quota2.

##### A sample Priority Class Deployment
```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: high-priority
value: 1000000
globalDefault: false
description: "Sample High Priority Class"
```

##### Deployment on Namespace quota2
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota2
  labels:
    app: nginx
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      schedulerName: capacityscheduler
      priorityClassName: high-priority
      containers:
        - name: nginx
          image: nginx
          resources:
            limits:
              cpu: 1
            requests:
              cpu: 1
```
In this case, the high priority pod in namespace quota2 cannot run even though it has a higher priority compared to the running pod in namespace quota1 because the capacity scheduler follows a restricted "in-namespace" preemption rule - pods within quota2 have overused their namespace resource min. 

### Known Limitations
#### ⚠️ Cross Node Preemption is not supported
Current default Kubernetes scheduler does not support cross node preemption, which means that preemption process only happens in one node. In order to make capacity scheduler compatible with the default scheduler, current implementation also does not support cross node preemption.

Because of that, it's expected in some cases the scheduler is incapable to give the resources back to the original namespace. This will lead to resource fragmentation. In the production environment, it is recommended to set the sum of min to be less than the total resources of the cluster. This can avoid this problem as much as possible.

Below is a simple example. Suppossed that you have **exactly** 2 nodes in your cluster with the following configuration:
##### Elastic Quota Configuration for namespace 1
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota1
  namespace: quota1
spec:
  max:
    cpu: 2
  min:
    cpu: 0
```
##### Elastic Quota Configuration for namespace 2
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: ElasticQuota
metadata:
  name: quota2
  namespace: quota2
spec:
  max:
    cpu: 2
  min:
    cpu: 2
```
##### Run Sample Deployment on namespace 1
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota1
  labels:
    app: nginx
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      schedulerName: capacityscheduler
      containers:
        - name: nginx
          image: nginx
          resources:
            limits:
              cpu: 1
            requests:
              cpu: 1
```
Suppose two replicas of the deployment get scheduled to Node 1 and Node 2 respectively.

Next, let's try to run another deployment on namespace 2.


##### Run Sample Deployment on namespace 2
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: quota2
  labels:
    app: nginx
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
    spec:
      schedulerName: capacityscheduler
      containers:
        - name: nginx
          image: nginx
          resources:
            limits:
              cpu: 2
            requests:
              cpu: 2
```
As you may have noticed, namespace quota1 overused its min quota, and has borrowed 2 cpus from quota2. Now here comes a pod from namespace quota2: ideally, the two pods in namespace quota1 should get preempted and the newly deployed pod on namespace quota2 should be scheduled. However, preemption is restricted to be attempted on one single node, so preempting either pod of namespace quota1 won't satisfy the incoming pod of quota2 - used cpus = 3 > 2 (sum of elastic quota min). Thus in this case, the preemption process fails no matter which node is selected.

### Test Plan

TBD

### Graduation Criteria

TBD

### Upgrade / Downgrade Strategy

TBD

### Version Skew Strategy

TBD

## Production Readiness Review Questionnaire

TBD

## Implementation History

TBD

## Drawbacks

TBD

## Alternatives

TBD
