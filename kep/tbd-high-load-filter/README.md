# KEP-TBD: High Load Filter

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User stories](#user-stories)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Plugin lifecycle and extension points](#plugin-lifecycle-and-extension-points)
  - [Metrics pipeline](#metrics-pipeline)
  - [Predicted utilization](#predicted-utilization)
  - [Filtering behavior](#filtering-behavior)
  - [API and configuration](#api-and-configuration)
  - [Example](#example)
  - [Validation and defaults](#validation-and-defaults)
  - [Security considerations](#security-considerations)
- [Scalability and performance](#scalability-and-performance)
- [Known limitations](#known-limitations)
- [Test Plan](#test-plan)
- [Graduation Criteria](#graduation-criteria)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
  - [Feature enablement and rollback](#feature-enablement-and-rollback)
  - [Rollout, upgrade, and rollback planning](#rollout-upgrade-and-rollback-planning)
  - [Monitoring requirements](#monitoring-requirements)
  - [Dependencies](#dependencies)
  - [Troubleshooting](#troubleshooting)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [References](#references)
<!-- /toc -->

## Summary

This proposal introduces a standalone scheduler framework plugin named
`HighLoadFilter`. It rejects nodes whose predicted CPU or memory utilization
would exceed operator-configured limits. Predicted utilization combines current
node utilization from load-watcher with the incoming Pod's resource requests.

The plugin implements `PreFilter` and `Filter`. `PreFilter` captures one
immutable metrics snapshot and the Pod requests for a scheduling cycle.
`Filter` performs only local calculations against that snapshot; it never makes
a remote request per candidate node.

HighLoadFilter does not depend on, enable, or modify any Trimaran plugin. It can
be used without `LoadVariationRiskBalancing`, `TargetLoadPacking`, or any other
load-aware score plugin. It reuses the load-watcher library and wire format only
as an independent metrics integration.

## Motivation

Resource-request-based filtering prevents declared overcommitment, but it does
not detect nodes whose workloads consume substantially more CPU or memory than
their requests suggest. Score plugins can prefer lower-load nodes, but scoring
cannot provide a hard placement boundary: if every feasible node is highly
loaded, one of those nodes can still receive the highest score.

Operators running latency-sensitive or availability-sensitive workloads need an
optional policy that removes highly loaded nodes from the feasible set before
scoring. The policy must also account for the incoming Pod so that a node just
below a threshold does not accept a Pod whose request immediately pushes the
predicted load above that threshold.

Trimaran KEP-61 deliberately lists a Filter implementation as a non-goal. This
proposal does not change that decision or add filtering behavior to Trimaran.
Instead, it defines a separate plugin with its own configuration, collector,
failure policy, documentation, and lifecycle.

### Goals

1. Provide optional hard filtering based on measured CPU and memory utilization.
2. Include the incoming Pod's effective CPU and memory requests in the decision.
3. Use one metrics snapshot consistently across all nodes in a scheduling cycle.
4. Support Kubernetes Metrics Server and Prometheus through load-watcher.
5. Support an independently deployed load-watcher service.
6. Define explicit behavior for missing, partial, stale, and unavailable metrics.
7. Keep all work in the scheduling cycle local and constant-time per node.
8. Operate independently of Trimaran and its plugins.

### Non-Goals

1. Score or rank nodes.
2. Change any Trimaran plugin or KEP-61 behavior.
3. Evict or reschedule Pods already running on a high-load node.
4. Predict actual future Pod consumption beyond Kubernetes resource requests.
5. Replace resource-request filters such as `NodeResourcesFit`.
6. Provide per-namespace, per-workload, or per-Pod threshold overrides in the
   initial version.
7. Exempt DaemonSets or other workload types implicitly.
8. Support SignalFx in the initial API.

## Proposal

Add `HighLoadFilter` to the scheduler-plugins binary and config API. Operators
enable it at both `PreFilter` and `Filter` in a scheduler profile.

At the beginning of a scheduling cycle, the plugin reads its most recent
load-watcher snapshot and stores it in `CycleState` together with the incoming
Pod's effective requests. Every `Filter` invocation in that cycle reads the same
state.

For each candidate node, the plugin extracts the average CPU and memory
utilization percentages and adds the incoming Pod request as a percentage of
node allocatable capacity. A predicted value greater than its configured limit
returns `Unschedulable`. Equality is allowed.

### User stories

1. As a cluster operator, I want to stop scheduling new Pods to nodes above 65%
   measured CPU utilization even if those nodes have unallocated requests.
2. As a scheduler operator, I want a Pod requesting 10% of a node to be rejected
   when the node is at 60% and my configured threshold is 65%.
3. As an availability-focused operator, I want to choose fail-closed behavior so
   missing or stale metrics cannot silently bypass the limit.
4. As a general-purpose cluster operator, I want fail-open behavior so a metrics
   outage cannot stop all scheduling.
5. As an existing load-watcher user, I want to use a separately managed watcher
   service without enabling any Trimaran plugin.

### Notes/Constraints/Caveats

- Metrics are expected to be percentages based on node capacity and keyed by
  exact Kubernetes node name.
- Pod requests are a conservative proxy for incremental utilization. Actual
  consumption may be lower or higher.
- CPU and memory thresholds apply cluster-wide within one scheduler profile.
- HighLoadFilter is complementary to request-based resource filters. It must not
  be used as their replacement.
- `Unschedulable` is used rather than `UnschedulableAndUnresolvable` because
  measured load and metrics availability can change without changing the Pod.
- No workload type receives an implicit bypass. Operators can use separate
  scheduler profiles when workloads require different policies.

### Risks and Mitigations

**Metrics outage can affect availability.** Fail-open is the default. Operators
that select fail-closed accept the explicit risk that a global metrics outage can
temporarily prevent scheduling.

**Stale metrics can produce incorrect decisions.** Every snapshot has a Unix
timestamp. Snapshots older than `nodeMetricExpirationSeconds` are handled using
the configured failure policy.

**Pod requests can overestimate incremental load.** This is intentional for a
hard safety boundary. Thresholds remain operator-controlled and default to 100%.

**Concurrent scheduling can admit several Pods based on the same measured
load.** Each scheduling cycle includes its own Pod request, but the plugin does
not reserve predicted utilization across concurrent cycles. Operators should
leave headroom in thresholds. A future enhancement could add an in-process
assumption cache after separate design review.

**A remote call could delay scheduler startup.** The external watcher client's
initial read uses the load-watcher client timeout. Subsequent scheduling cycles
use only the local cache. Operators should keep the watcher close to the
scheduler and use normal scheduler readiness supervision.

**Embedded load-watcher has a process-wide HTTP listener.** This is inherited
from the load-watcher library. Multi-profile or multi-consumer configurations
should use the external service mode.

**Clock skew can make freshness checks inaccurate.** Scheduler and metrics
source clocks should be synchronized. Timestamp and snapshot age are logged for
troubleshooting without logging credentials.

## Design Details

### Plugin lifecycle and extension points

HighLoadFilter implements:

- `PreFilterPlugin` to capture the snapshot and calculate Pod requests;
- `FilterPlugin` to evaluate each node;
- `SignPlugin` so Pods with identical CPU and memory requests remain eligible
  for scheduler batching optimizations.

The plugin's collector performs an initial update during construction and then
refreshes its cached snapshot on a context-bound ticker. Failed refreshes retain
the last successful snapshot; expiration prevents that snapshot from being used
forever.

`PreFilter` stores the following immutable data in `CycleState`:

```go
type preFilterState struct {
    metrics     *watcher.WatcherMetrics
    podRequests corev1.ResourceList
    unavailable string
}
```

`Filter` returns an error when the state is missing, making an extension-point
misconfiguration visible instead of silently losing snapshot consistency.

### Metrics pipeline

HighLoadFilter has its own collector and config types. It imports load-watcher
directly and does not import `pkg/trimaran`.

Two mutually exclusive modes are supported:

1. **Service mode:** `watcherAddress` points to an independently deployed
   load-watcher HTTP service.
2. **Embedded mode:** `metricProvider` configures load-watcher's library client
   for Kubernetes Metrics Server or Prometheus.

The collector replaces the cached snapshot atomically after a successful
refresh. It never clears a successful snapshot on a transient refresh error.

### Predicted utilization

For resource `r` on node `n` and incoming Pod `p`:

```text
requestPercent(r, p, n) = request(r, p) / allocatable(r, n) * 100
predicted(r, p, n) = measured(r, n) + requestPercent(r, p, n)
```

Effective Pod requests are calculated with the Kubernetes component helper,
which includes normal container aggregation, init-container semantics, Pod-level
resources where applicable, and Pod overhead.

If allocatable capacity is non-positive and the Pod request is positive, the
predicted value is positive infinity and the node is rejected. The in-tree
resource filters are still expected to reject such a node independently.

For compatibility with existing load-watcher responses, the plugin prefers an
`AVG` metric and falls back to `Latest` or an operator-less value. Negative,
NaN, and infinite input values are treated as unavailable.

### Filtering behavior

The behavior for each resource is:

1. If a valid metric exists, enforce its threshold.
2. If the valid metric exceeds the threshold, reject the node even when the
   other resource metric is missing and fail-open is selected.
3. After enforcing all available metrics, apply the failure policy if either
   required resource is missing.

| Condition | `failOpen: true` | `failOpen: false` |
|---|---|---|
| Snapshot unavailable | allow | reject |
| Snapshot timestamp missing | allow | reject |
| Snapshot stale | allow | reject |
| Node missing from snapshot | allow | reject |
| CPU or memory metric missing | enforce available metrics, then allow | reject |
| Predicted CPU exceeds threshold | reject | reject |
| Predicted memory exceeds threshold | reject | reject |

### API and configuration

The plugin introduces these scheduler-plugins config API types:

```go
type HighLoadFilterMetricProviderType string

const (
    HighLoadFilterKubernetesMetricsServer HighLoadFilterMetricProviderType = "KubernetesMetricsServer"
    HighLoadFilterPrometheus              HighLoadFilterMetricProviderType = "Prometheus"
)

type HighLoadFilterMetricProviderSpec struct {
    Type               HighLoadFilterMetricProviderType
    Address            string
    Token              string
    InsecureSkipVerify bool
}

type HighLoadFilterUsageThresholds struct {
    CPU    int64
    Memory int64
}

type HighLoadFilterArgs struct {
    WatcherAddress                  string
    MetricProvider                  HighLoadFilterMetricProviderSpec
    UsageThresholds                 HighLoadFilterUsageThresholds
    FailOpen                        bool
    MetricsUpdateIntervalSeconds    int64
    NodeMetricExpirationSeconds     int64
}
```

The versioned API uses pointers for fields where omission must be distinguished
from an explicit zero or false value.

### Example

External load-watcher service:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
- schedulerName: high-load-aware-scheduler
  plugins:
    preFilter:
      enabled:
      - name: HighLoadFilter
    filter:
      enabled:
      - name: HighLoadFilter
  pluginConfig:
  - name: HighLoadFilter
    args:
      watcherAddress: http://load-watcher.monitoring.svc:2020
      usageThresholds:
        cpu: 65
        memory: 95
      failOpen: true
      metricsUpdateIntervalSeconds: 30
      nodeMetricExpirationSeconds: 180
```

Embedded Prometheus mode:

```yaml
pluginConfig:
- name: HighLoadFilter
  args:
    metricProvider:
      type: Prometheus
      address: https://prometheus.monitoring.svc:9090
      insecureSkipVerify: false
    usageThresholds:
      cpu: 65
      memory: 95
```

### Validation and defaults

| Field | Default | Validation |
|---|---:|---|
| `watcherAddress` | empty | absolute HTTP(S) URL; mutually exclusive with `metricProvider` |
| `metricProvider.type` | `KubernetesMetricsServer` | `KubernetesMetricsServer` or `Prometheus` |
| `metricProvider.address` | empty | required absolute HTTP(S) URL for Prometheus |
| `metricProvider.insecureSkipVerify` | `false` | unsupported for Kubernetes Metrics Server |
| `usageThresholds.cpu` | `100` | `[0, 100]` |
| `usageThresholds.memory` | `100` | `[0, 100]` |
| `failOpen` | `true` | boolean |
| `metricsUpdateIntervalSeconds` | `30` | greater than zero |
| `nodeMetricExpirationSeconds` | `180` | at least the update interval |

Thresholds default to 100% to avoid introducing environment-specific operating
limits. Operators that need protective headroom must configure lower values.

### Security considerations

- The Prometheus token is never logged.
- Validation errors redact the metric provider object so a token is not exposed
  in scheduler startup errors.
- `insecureSkipVerify` defaults to false.
- Scheduler configuration containing a token must be protected as a secret and
  limited to the scheduler service account and cluster administrators.
- The plugin does not read annotations or owner references to bypass filtering,
  avoiding a workload-controlled policy escape.
- TLS and authentication for an external load-watcher service are deployment
  concerns; operators should prefer a trusted in-cluster endpoint or service
  mesh policy.

## Scalability and performance

Remote metrics access occurs only at the configured refresh interval, not once
per Pod or node. A successful response contains the cluster node map and is
stored as a single immutable snapshot.

For a scheduling cycle with `N` candidate nodes:

- `PreFilter`: one cache read and one Pod request calculation;
- `Filter`: `O(1)` node-map lookup and a bounded scan of CPU/memory metrics per
  node;
- total plugin work: `O(N)` with no per-node network I/O.

The initial prototype has been functionally exercised in a real 115-node
cluster. This is operational evidence, not a formal performance benchmark.
Alpha graduation requires repeatable benchmarks for PreFilter latency, Filter
latency, allocation impact, and scheduler throughput at representative node
counts.

## Known limitations

1. Concurrent cycles do not reserve predicted utilization between metrics
   refreshes.
2. Pod requests are not an actual-consumption prediction model.
3. One configuration applies to every Pod using the scheduler profile.
4. Snapshot expiration depends on clock synchronization.
5. A change in measured load does not create a Kubernetes cluster event. Pods
   are retried through normal scheduler retry behavior.
6. Embedded load-watcher lifecycle and its process-wide HTTP listener are
   inherited from the upstream load-watcher library.
7. Only CPU and memory are supported initially.

## Test Plan

Unit tests cover:

- CPU and memory below, equal to, and above thresholds;
- a Pod request that moves predicted utilization across each threshold;
- unavailable, stale, node-missing, and partially missing metrics;
- fail-open and fail-closed behavior;
- invalid metric values and average/latest selection;
- one immutable snapshot per scheduling cycle;
- missing PreFilter state and canceled contexts;
- collector refresh success, error retention, and context cancellation;
- defaults, API conversion/deepcopy, strict decoding, and validation;
- plugin name, scheduler batching signature, and scheduler registration.

An envtest integration test runs a scheduler with HighLoadFilter, creates a
low-load and a high-load node, and verifies that a Pod is bound only to the
low-load node.

Before implementation merge, CI must pass code generation verification,
formatting, static analysis, unit tests, and integration tests.

## Graduation Criteria

**Alpha**

- KEP accepted.
- Config API, validation, defaults, and generated code included.
- Unit and envtest integration coverage from the test plan included.
- Plugin and operations documentation published.
- No default scheduler profile enables the plugin.

**Beta**

- At least two independent production reports.
- Repeatable scale benchmarks and documented resource cost.
- Prometheus metrics for snapshot age, refresh failures, missing node metrics,
  and filter rejection reasons.
- E2E coverage for metrics outage and recovery.
- Review concurrent-cycle admission behavior and determine whether an assumption
  cache is required.

## Production Readiness Review Questionnaire

### Feature enablement and rollback

**How can this feature be enabled or disabled in a live cluster?**

Enable it by adding HighLoadFilter to both `preFilter` and `filter` in a
scheduler profile. Disable it by removing it from both extension points and
rolling out the scheduler configuration.

**Does enabling it change default behavior?**

No. The plugin is not enabled in any default profile.

**What happens when it is disabled?**

Scheduling returns to the remaining configured filters. No data migration or
node change is required.

### Rollout, upgrade, and rollback planning

Operators should begin with fail-open and thresholds near 100, observe rejection
and snapshot-age metrics, and then lower thresholds. Fail-closed should be used
only after the metrics pipeline has demonstrated sufficient availability.

Rollback consists of removing the plugin configuration and extension-point
registrations. The collector stores no persistent data.

### Monitoring requirements

Alpha troubleshooting uses structured scheduler logs. Before beta, the plugin
must expose counters and gauges for:

- refresh successes and failures;
- current snapshot age;
- unavailable/stale/node-missing metrics;
- CPU and memory threshold rejections;
- fail-open decisions.

Alerts should cover sustained refresh failures and snapshot age approaching the
configured expiration.

### Dependencies

- load-watcher `v0.2.4` library or compatible service response;
- Kubernetes Metrics Server or Prometheus for embedded mode;
- synchronized scheduler and metrics-source clocks.

A metrics backend or external watcher outage is handled by `failOpen`. The
scheduler process remains running in either failure policy.

### Troubleshooting

Operators should check:

1. load-watcher health and scheduler connectivity;
2. snapshot timestamp and configured expiration;
3. exact match between load-watcher host keys and Kubernetes node names;
4. presence of both CPU and memory metrics;
5. node allocatable capacity and incoming Pod requests;
6. that HighLoadFilter is enabled at both extension points;
7. threshold rejection and fail-open log fields.

## Implementation History

- 2026-07-20: initial KEP and standalone prototype prepared.

## Drawbacks

- Another filter can make Pods unschedulable during load spikes.
- Conservative request addition can reduce bin packing.
- The scheduler gains an external operational dependency when fail-closed is
  used.
- The initial implementation duplicates a small load-watcher configuration and
  collector abstraction instead of sharing Trimaran code; this is intentional
  to keep plugin ownership and behavior independent.

## Alternatives

1. **Add Filter to LoadVariationRiskBalancing.** Rejected because it couples a
   hard admission policy to a score plugin, changes KEP-61's stated non-goals,
   and prevents independent use.
2. **Reuse `pkg/trimaran.Collector`.** Rejected because it makes the new plugin
   depend on Trimaran API and lifecycle semantics. HighLoadFilter imports
   load-watcher directly instead.
3. **Use only current utilization.** Rejected because a node just below a limit
   could accept a large Pod and immediately cross it.
4. **Publish load as node labels and use node affinity.** This adds API-server
   write load, loses snapshot timestamps and partial-metric semantics, and
   requires a separate controller.
5. **Rely only on score plugins.** Scoring cannot provide a hard upper bound when
   every candidate is highly loaded.
6. **Fail closed unconditionally.** Rejected because a metrics outage could halt
   cluster scheduling with no operator choice.

## References

- [Scheduler Plugins](https://github.com/kubernetes-sigs/scheduler-plugins)
- [Trimaran KEP-61](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/61-Trimaran-real-load-aware-scheduling)
- [load-watcher](https://github.com/paypal/load-watcher)
- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)
