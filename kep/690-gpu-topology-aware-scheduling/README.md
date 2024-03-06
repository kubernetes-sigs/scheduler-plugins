# KEP-690: GPU Topology Aware

<!--
This is the title of your KEP. Keep it short, simple, and descriptive. A good
title can help communicate what the KEP is and should be considered as part of
any review.
-->

<!--
A table of contents is helpful for quickly jumping to sections of a KEP and for
highlighting any additional information provided beyond the standard KEP
template.

Ensure the TOC is wrapped with
  <code>&lt;!-- toc --&rt;&lt;!-- /toc --&rt;</code>
tags, and then generate with `hack/update-toc.sh`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
    - [Story 1](#story-1)
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Test Plan](#test-plan)
      - [Prerequisite testing updates](#prerequisite-testing-updates)
      - [Unit tests](#unit-tests)
      - [Integration tests](#integration-tests)
      - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
    - [Alpha](#alpha)
    - [Beta](#beta)
- [Implementation History](#implementation-history)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

<!--
This section is incredibly important for producing high-quality, user-focused
documentation such as release notes or a development roadmap. It should be
possible to collect this information before implementation begins, in order to
avoid requiring implementors to split their attention between writing release
notes and implementing the feature itself. KEP editors and SIG Docs
should help to ensure that the tone and content of the `Summary` section is
useful for a wide audience.

A good summary is probably at least a paragraph in length.

Both in this section and below, follow the guidelines of the [documentation
style guide]. In particular, wrap lines to a reasonable length, to make it
easier for reviewers to cite specific portions, and to minimize diff churn on
updates.

[documentation style guide]: https://github.com/kubernetes/community/blob/master/contributors/guide/style-guide.md
-->

GPUs today are the essential accelerators for HPC/AI workloads, especially in GenAI era. And the
substantial time spent in P2P communication between GPUs can prevent researchers/engineers from
maximizing the potential performance from these devices. The biggest vendor, Nvidia, developed
NVLink/NVSwitch to mitigate the impact. As a result, the bandwidth between PCIe and NVLink or
between different num of NVLinks can vary a lot.

As an user, I hope my GPU accelerated workloads can be placed with the best effort allocations between GPUs and nodes
across the cluster, which will speed up the experimental a lot.

## Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this KEP.  Describe why the change is important and the benefits to users. The
motivation section can optionally provide links to [experience reports] to
demonstrate the interest in a KEP within the wider Kubernetes community.

[experience reports]: https://github.com/golang/go/wiki/ExperienceReports
-->

The GPU-to-GPU (we only consider Nvidia GPU here) link type differs a lot, like `SYS`, `NODE`, `NV#`,
and generally the bandwidth is `NV#` > `NODE` > `SYS`. You can run `nvidia-smi typo -m` to see the matrix.
A simplified example as blow:

```cmd
        GPU0   GPU1   GPU2   GPU3   GPU4   GPU5   GPU6   GPU7
GPU0     X     NV1    NV1    NV2    NV2    SYS    SYS    SYS
GPU1    NV1     X     NV2    NV1    SYS    NV2    SYS    SYS
GPU2    NV1    NV2     X     NV2    SYS    SYS    NV1    SYS
GPU3    NV2    NV1    NV2     X     SYS    SYS    SYS    NV1
GPU4    NV2    SYS    SYS    SYS     X     NV1    NV1    NV2
GPU5    SYS    NV2    SYS    SYS    NV1     X     NV2    NV1
GPU6    SYS    SYS    NV1    SYS    NV1    NV2     X     NV2
GPU7    SYS    SYS    SYS    NV1    NV2    NV1    NV2     X


Legend:

  X    = Self
  SYS  = Connection traversing PCIe as well as the SMP interconnect between NUMA nodes (e.g., QPI/UPI)
  NODE = Connection traversing PCIe as well as the interconnect between PCIe Host Bridges within a NUMA node
  PHB  = Connection traversing PCIe as well as a PCIe Host Bridge (typically the CPU)
  PXB  = Connection traversing multiple PCIe bridges (without traversing the PCIe Host Bridge)
  PIX  = Connection traversing at most a single PCIe bridge
  NV#  = Connection traversing a bonded set of # NVLinks
```

And for `NV#` type, the more NVLinks the higher bandwidth, so NV2 > NV1. When scheduling pods with GPUs, we hope
the scheduler can be aware of the GPU topology, and allocate Pods to the most suitable Nodes with higher bandwidth
between GPUs.

### Goals

<!--
List the specific goals of the KEP. What is it trying to achieve? How will we
know that this has succeeded?
-->

- Single Pod requests multi GPUs will pick the most suitable node for acceleration based on the P2P link type.
- Work smoothly with other components, e.g. device plugin.

### Non-Goals

<!--
What is out of scope for this KEP? Listing non-goals helps to focus discussion
and make progress.
-->

- Opt for the specific GPUs in scheduler
- Multi-GPU & multi-node distributed training scene support
- vGPU or MIG scene support
- DRA support
- Better algorithms in allocating GPUs, this is what [go-gpuallocator](https://github.com/NVIDIA/go-gpuallocator) does.

Note: we may support all these goals in the long-term, but not this time.

## Proposal

<!--
This is where we get down to the specifics of what the proposal actually is.
This should have enough detail that reviewers can understand exactly what
you're proposing, but should not include things like API designs or
implementation. What is the desired outcome and how do we measure success?.
The "Design Details" section below is for the real
nitty-gritty.
-->

### User Stories (Optional)

<!--
Detail the things that people will be able to do if this KEP is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

#### Story 1

We have two nodes, each node has eight Tesla V100, and Node-1 is equipped with
NVLinks, but not Node-2.
For Node-1, each GPU has up to 6 NVLinks, since we can not reach full-mesh
between all devices, so we configured the NVLink as blow:

Take NV0 for example, we can found that:

- NV0 & NV1, NV0 & NV2 has two NVLinks
- NV0 & NV3, NV0 & NV5 has one NVLinks
- NV0 & NV4, NV0 & NV6, NV0 & NV7 has no NVLinks

```graph
    +---------------------------------------------------------+
    |                                                         |
    v                                                         v
+-------+       +-------+                 +-------+       +-------+
|  NV0  |  <->  |  NV1  | <-------------> |  NV4  |  <->  |  NV5  |
|       |  <->  |       | <-------------> |       |  <->  |       |
+-------+       +-------+                 +-------+       +-------+
   ^ ^   ^     ^   ^                         ^ ^   ^     ^   ^ ^
   | |    \   /    |                         | |    \   /    | |
   | |      X      |                         | |      X      | |
   | |     /  \    |                         | |     /  \    | |
   v v   v     v   v                         v v   v     v   v v
+-------+       +-------+                 +-------+       +-------+
|  NV2  |  <->  |  NV3  | <-------------> |  NV6  |  <->  |  NV7  |
|       |  <->  |       | <-------------> |       |  <->  |       |
+-------+       +-------+                 +-------+       +-------+
    ^                                                         ^
    |                                                         |
    +---------------------------------------------------------+
```

Then when scheduling a pod with 2 GPUs, we hope to pick Node-1 rather than Node-2 and
GPUS for NV0 & NV1 (we have other choices here) rather than NV0 & NV6

### Notes/Constraints/Caveats (Optional)

<!--
What are the caveats to the proposal?
What are some important details that didn't come across above?
Go in to as much detail as necessary here.
This might be a good place to talk about core concepts and how they relate.
-->

Considering NVLink has different generations as well as PCIe, so just based on the link type
or NVLink number may be not enough, we can use bandwidth library for experimental, but we dropped
this approach for the reason under [Alternatives](#alternatives)

### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate? Think broadly.
For example, consider both security and how this will impact the larger
Kubernetes ecosystem.

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->

## Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

**Architecture**

![architecture](image.png)

This is how the architecture generally looks like, as you see, we need several components to work together.

- Kube-scheduler: with new implemented plugin to help finding the most suitable node with satisfied GPUs
- kubelet & k8s-device-plugin: work as before
- [GFD](https://github.com/NVIDIA/gpu-feature-discovery) & [NFD](https://github.com/kubernetes-sigs/node-feature-discovery): patch GPU topology matrix to Node annotation, currently, it's not supported yet, see <https://github.com/NVIDIA/k8s-device-plugin/issues/465>,
we'll help to implement this.

**How to pick the best effort GPUs in one node**

This is implemented by default inside k8s-device-plugin, which leverages the library of [go-gpuallocator](https://github.com/NVIDIA/go-gpuallocator).
Generally, go-gpuallocator will try to find a set of GPUs with the highest bandwidth. The bandwidth is scored by the link type,
see details [here](https://github.com/NVIDIA/go-gpuallocator/blob/8fc3087147de2bf654ee91929e9a70d754c6bae1/gpuallocator/besteffort_policy.go#L316-L367).

Note that go-gpuallocator will not do actual allocation, this is what k8s-device-plusin does. go-gpuallocator will only dry-run
to find the best set of GPUs.

The allocation algorithm is:

- We'll have a map of P2P link type to Score, like `SingleNVLINKLink` values 100 scores,`TwoNVLINKLinks` values 200 scores.
- When we require X GPUs for Pod, we'll get M possibles partitions with size of X. And calculate the total scores of each partitions.
For example, we have GPU0, GPU1, GPU2, GPU3 and request 3 GPUs, we'll have Partition0[[GPU0, GPU1, GPU2], [GPU3, nil, nil]],
Partition1[[GPU0, GPU1, GPU3], [GPU2, nil, nil]], Partition2[[GPU1, GPU2, GPU3], [GPU0, nil, nil]], the score of Partition0 is
sum(score(GPU0, GPU1), score(GPU0, GPU2), score(GPU1, GPU2), score(GPU3, nil)).
- Assume Partition0 has the highest score, then we'll pick a set of the partition who has the highest score. Here is the [GPU0, GPU1, GPU2].

**How to pick the most suitable Node across cluster**

Kube-scheduler will follow the same algorithm of go-gpuallocator, it will process all possible nodes and
calculate the scores of each Node.

For scheduler-plugin, we'll implement `PreScore` and `Score` extension points:

- PreScore: If Pod doesn't require `nvidia.com/gpu`, return SKip directly.
- Score: reuse the go-gpuallocator's scoring algorithm to get the original score, then for normalizing,

    ```golang
    maxScore = limitQuota * (limitQuota - 1) * 1800 / 2 // 1800 is the maximum score between two GPUs, on behalf of 18 NVLinks
    finalScore = originalScore * 100 / maxScore
    ```

A potential issue here is if the score of the link type is too small, like `P2PLinkCrossCPU` maps to 10, then we may loss the precision here
and got 0 score finally.

**In summary**

Based on the same scoring algorithm, kube-scheduler will find the best-fit node, but no need to tell the device plugin which GPUs are they,
the k8s-device-plugin can find them accordingly and do the actual allocation.

However, the go-gpuallocator doesn't support returning score yet, we'd like to support this in the future, see <https://github.com/NVIDIA/go-gpuallocator/issues/19>.
Now, we're reusing part of the source code.

### Test Plan

<!--
**Note:** *Not required until targeted at a release.*
The goal is to ensure that we don't accept enhancements with inadequate testing.

All code is expected to have adequate tests (eventually with coverage
expectations). Please adhere to the [Kubernetes testing guidelines][testing-guidelines]
when drafting this test plan.

[testing-guidelines]: https://git.k8s.io/community/contributors/devel/sig-testing/testing.md
-->

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

<!--
Based on reviewers feedback describe what additional tests need to be added prior
implementing this enhancement to ensure the enhancements have also solid foundations.
-->

##### Unit tests

<!--
In principle every added code should have complete unit test coverage, so providing
the exact set of tests will not bring additional value.
However, if complete unit test coverage is not possible, explain the reason of it
together with explanation why this is acceptable.
-->

<!--
Additionally, for Alpha try to enumerate the core package you will be touching
to implement this enhancement and provide the current unit coverage for those
in the form of:
- <package>: <date> - <current test coverage>
The data can be easily read from:
https://testgrid.k8s.io/sig-testing-canaries#ci-kubernetes-coverage-unit

This can inform certain test coverage improvements that we want to do before
extending the production code to implement this enhancement.
-->

- Unsatisfied GPU topology aware test, like no topology provided or Pod doesn't require GPUs
- GPU allocate tests with different number of GPUs requested
- Kube-scheduler prescore & score tests with different number of GPUs requested

##### Integration tests

<!--
Integration tests are contained in k8s.io/kubernetes/test/integration.
Integration tests allow control of the configuration parameters used to start the binaries under test.
This is different from e2e tests which do not allow configuration of parameters.
Doing this allows testing non-default options and multiple different and potentially conflicting command line options.
-->

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html
-->

- Pod scheduling test
  - with different set of nodes provided
  - with different GPUs requested

##### e2e tests

<!--
This question should be filled when targeting a release.
For Alpha, describe what tests will be added to ensure proper quality of the enhancement.

For Beta and GA, add links to added tests together with links to k8s-triage for those tests:
https://storage.googleapis.com/k8s-triage/index.html

We expect no non-infra related flakes in the last month as a GA graduation criteria.
-->

Integration-tests is enough for we only involve scheduler here.

### Graduation Criteria

<!--
**Note:** *Not required until targeted at a release.*

Define graduation milestones.

These may be defined in terms of API maturity, [feature gate] graduations, or as
something else. The KEP should keep this high-level with a focus on what
signals will be looked at to determine graduation.

Consider the following in developing the graduation criteria for this enhancement:
- [Maturity levels (`alpha`, `beta`, `stable`)][maturity-levels]
- [Feature gate][feature gate] lifecycle
- [Deprecation policy][deprecation-policy]

Clearly define what graduation means by either linking to the [API doc
definition](https://kubernetes.io/docs/concepts/overview/kubernetes-api/#api-versioning)
or by redefining what graduation means.

In general we try to use the same stages (alpha, beta, GA), regardless of how the
functionality is accessed.

[feature gate]: https://git.k8s.io/community/contributors/devel/sig-architecture/feature-gates.md
[maturity-levels]: https://git.k8s.io/community/contributors/devel/sig-architecture/api_changes.md#alpha-beta-and-stable-versions
[deprecation-policy]: https://kubernetes.io/docs/reference/using-api/deprecation-policy/

Below are some examples to consider, in addition to the aforementioned [maturity levels][maturity-levels].

#### Alpha

- Feature implemented behind a feature flag
- Initial e2e tests completed and enabled

#### Beta

- Gather feedback from developers and surveys
- Complete features A, B, C
- Additional tests are in Testgrid and linked in KEP

#### GA

- N examples of real-world usage
- N installs
- More rigorous forms of testing—e.g., downgrade tests and scalability tests
- Allowing time for feedback

**Note:** Generally we also wait at least two releases between beta and
GA/stable, because there's no opportunity for user feedback, or even bug reports,
in back-to-back releases.

**For non-optional features moving to GA, the graduation criteria must include
[conformance tests].**

[conformance tests]: https://git.k8s.io/community/contributors/devel/sig-architecture/conformance-tests.md

#### Deprecation

- Announce deprecation and support policy of the existing flag
- Two versions passed since introducing the functionality that deprecates the flag (to address version skew)
- Address feedback on usage/changed behavior, provided on GitHub issues
- Deprecate the flag
-->

#### Alpha

- Scheduler plugin implemented
- Unit test & integration tests provided

#### Beta

- Gather feedback from developers and surveys
- (TBD) We may need to implement more features to meet different scenarios.

## Implementation History

<!--
Major milestones in the lifecycle of a KEP should be tracked in this section.
Major milestones might include:
- the `Summary` and `Motivation` sections being merged, signaling SIG acceptance
- the `Proposal` section being merged, signaling agreement on a proposed design
- the date implementation started
- the first Kubernetes release where an initial version of the KEP was available
- the version of Kubernetes where the KEP graduated to general availability
- when the KEP was retired or superseded
-->

- 2023-12-26: KEP created

## Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

- DRA is also what we considered since it's more flexible, but it's alpha now and
in the future, this plugin can also be extended, we didn't block up the possibility.
- We also experimented with [bandwidth](https://github.com/NVIDIA/nvbandwidth) library for accurate
bandwidth measurements, but this requires pre-install actions and sometimes will block the GPU allocation,
we have to restart the Pod then. So we take it as not matured or just use it under bad practices.
- We used to do GPU prioritizing in scheduler side, however, we have to fork the device plugin and do a lot
additional works, so we dropped as well, we want to follow the upstream.
