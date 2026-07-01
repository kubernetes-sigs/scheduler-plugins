# KEP-942: Node Metadata Scoring

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Use cases](#use-cases)
  - [Cost-aware placement](#cost-aware-placement)
  - [Freshness-aware placement](#freshness-aware-placement)
  - [Hardware lifecycle placement](#hardware-lifecycle-placement)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API and configuration](#api-and-configuration)
  - [Example](#example)
  - [Plugin registration and extension point](#plugin-registration-and-extension-point)
  - [Score calculation](#score-calculation)
    - [Numeric metadata](#numeric-metadata)
    - [Timestamp metadata](#timestamp-metadata)
    - [Normalization](#normalization)
  - [Validation and defaults](#validation-and-defaults)
- [Known limitations](#known-limitations)
- [Test plan](#test-plan)
- [Graduation criteria](#graduation-criteria)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
  - [Feature enablement and rollback](#feature-enablement-and-rollback)
  - [Scalability](#scalability)
  - [Troubleshooting](#troubleshooting)
- [Implementation History](#implementation-history)
- [Alternatives](#alternatives)
- [References](#references)
<!-- /toc -->

## Summary

This proposal introduces a new scheduler Score plugin, `NodeMetadata`, that
ranks feasible nodes by reading a single node label or annotation and
interpreting its value as either a number or a timestamp. It lets cluster
operators express preferences such as lower cost, newer hardware, older stable
nodes, or recent maintenance without adding new APIs to Kubernetes core
objects.

The plugin is optional and composable. It participates only in scoring, uses
the standard scheduler framework score range `[0, 100]`, and can be combined
with other scheduler plugins through normal score weights.

## Motivation

Many clusters already attach operational metadata to nodes through labels and
annotations. Those values often carry placement intent:

- cost per hour for spot and on-demand nodes
- hardware generation or performance tier
- creation, provisioning, or maintenance timestamps
- custom business priorities managed outside the scheduler

Today, users can express hard placement constraints with node affinity or
taints, but those mechanisms do not provide a generic way to rank candidate
nodes by arbitrary metadata. Existing in-tree and out-of-tree scoring plugins
mostly focus on resources, topology, or live utilization, not operator-managed
metadata.

This proposal fills that gap with a scoring plugin that turns existing node
metadata into a scheduling preference while preserving the scheduler framework
model.

### Goals

1. Provide a generic score plugin that ranks nodes using one configurable node
   metadata key.
2. Support both numeric metadata and timestamp metadata.
3. Support common ordering modes: `Highest`, `Lowest`, `Newest`, and `Oldest`.
4. Keep the plugin easy to configure, validate, and combine with other scoring
   plugins.
5. Reuse existing node labels and annotations instead of requiring a new CRD or
   new node API fields.

### Non-Goals

1. Introduce new Kubernetes API fields on `Node`.
2. Manage, reconcile, or update metadata values on behalf of operators.
3. Use metadata for filtering or hard admission guarantees.
4. Support multi-key expressions, formulas, or weighted combinations of several
   metadata fields in the initial version.
5. Reschedule or deschedule already running pods when node metadata changes.

## Use cases

### Cost-aware placement

Operators can annotate nodes with hourly cost information and configure the
plugin to prefer the lowest-cost feasible nodes without making cost a hard
constraint.

### Freshness-aware placement

Platform teams can annotate nodes with maintenance timestamps and prefer nodes
that were maintained most recently.

### Hardware lifecycle placement

Infrastructure operators can label nodes with provisioning dates or generation
numbers and then choose whether a scheduler profile should prefer older stable
nodes or newer hardware.

## Proposal

Add a new scheduler plugin named `NodeMetadata`. The plugin reads a configured
node label or annotation and converts its value into a raw score:

- numeric metadata can prefer higher or lower values
- timestamp metadata can prefer newer or older values

After raw scores are computed for all candidate nodes, the plugin normalizes
them to the scheduler framework range `[framework.MinNodeScore,
framework.MaxNodeScore]`.

The model stays small: one metadata key, one source, one value type, and one
scoring strategy per plugin configuration. That keeps configuration and
behavior predictable.

### Notes/Constraints/Caveats

- The plugin is a scoring-only mechanism. It does not enforce placement on a specific node.
- Metadata remains stored as strings on the `Node` object, so correctness
  depends on how operators publish and maintain those values.
- A single plugin configuration evaluates only one metadata key. If operators
  need more complex policies, they must compose this plugin with other scheduler
  plugins or use separate scheduler profiles.
- Missing or malformed metadata does not fail scheduling; it reduces the
  influence of this plugin for that node.

### Risks and Mitigations

- Stale or inconsistent metadata can lead to poor ranking decisions. Operators
  need to treat metadata publication as part of their node-management workflow.
- Putting too much weight on one metadata dimension can cause undesirable
  packing. The plugin uses standard scheduler weights, so operators can tune or
  reduce its impact relative to other score plugins.
- Metadata quality can vary across nodes. Nodes without the configured key, or
  with invalid values, receive the lowest effective influence from this plugin
  instead of causing a scheduling failure.

## Design Details

### API and configuration

The plugin introduces `NodeMetadataArgs` in the scheduler-plugins config API.

```go
type MetadataSourceType string

const (
    MetadataSourceLabel      MetadataSourceType = "Label"
    MetadataSourceAnnotation MetadataSourceType = "Annotation"
)

type MetadataValueType string

const (
    MetadataTypeNumber    MetadataValueType = "Number"
    MetadataTypeTimestamp MetadataValueType = "Timestamp"
)

type MetadataScoringStrategy string

const (
    ScoringStrategyHighest MetadataScoringStrategy = "Highest"
    ScoringStrategyLowest  MetadataScoringStrategy = "Lowest"
    ScoringStrategyNewest  MetadataScoringStrategy = "Newest"
    ScoringStrategyOldest  MetadataScoringStrategy = "Oldest"
)

type NodeMetadataArgs struct {
    MetadataKey     string
    MetadataSource  MetadataSourceType
    MetadataType    MetadataValueType
    ScoringStrategy MetadataScoringStrategy
    TimestampFormat string
}
```

Configuration fields:

| Field | Required | Description |
|---|---|---|
| `metadataKey` | Yes | Label or annotation key to evaluate |
| `metadataSource` | Yes | `Label` or `Annotation` |
| `metadataType` | Yes | `Number` or `Timestamp` |
| `scoringStrategy` | Yes | `Highest`, `Lowest`, `Newest`, or `Oldest` |
| `timestampFormat` | Conditional | Go time layout used when `metadataType` is `Timestamp` |

### Example

Example scheduler configuration:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: cost-aware-scheduler
    plugins:
      score:
        enabled:
          - name: NodeMetadata
            weight: 10
    pluginConfig:
      - name: NodeMetadata
        args:
          metadataKey: "node.example.com/hourly-cost"
          metadataSource: "Annotation"
          metadataType: "Number"
          scoringStrategy: "Lowest"
```

### Plugin registration and extension point

The plugin is registered in the scheduler binary and implements the scheduler
framework `ScorePlugin` interface. It also implements `ScoreExtensions` to
normalize results across all candidate nodes.

The initial design uses only the `Score` extension point. It does not require a
custom `PreScore`, `Filter`, `Reserve`, or cache component because all
information is read directly from the current `Node` object supplied during
scoring.

### Score calculation

For each candidate node, the plugin:

1. Reads the configured key from either `node.Labels` or `node.Annotations`.
2. Parses the string value according to the configured metadata type.
3. Converts the parsed value into a raw score whose ordering matches the chosen
   strategy.
4. Returns raw scores for normalization across all feasible nodes.

If the key is missing or the value cannot be parsed, the plugin returns an
internal invalid score sentinel for that node. During normalization, invalid
scores are excluded from the valid score range and are normalized to
`framework.MinNodeScore`. Valid scores are normalized above that reserved
minimum, so nodes with valid metadata rank ahead of nodes where metadata is
absent or malformed.

#### Numeric metadata

Numeric metadata is parsed from the string value as a `float64`, rounded to the
nearest `int64`, and converted into a raw score.

- `Highest`: larger numeric values produce larger raw scores.
- `Lowest`: smaller numeric values produce larger normalized scores by inverting
  the raw score before normalization.

This mode supports common use cases such as cost tiers, business priorities,
performance scores, or Unix epoch values encoded as numbers.

Because the scheduler framework score path uses `int64`, fractional metadata
values can lose precision during rounding. Operators that need exact ordering
should use integer metadata values.

#### Timestamp metadata

Timestamp metadata is parsed using a Go time layout. The plugin converts the
parsed timestamp into an age-based raw score:

- `Newest`: more recent timestamps receive larger effective scores.
- `Oldest`: older timestamps receive larger effective scores.

This mode is intended for values such as provisioning time, maintenance time,
and refresh time.

#### Normalization

After all raw scores are collected, the plugin normalizes them linearly into the
standard scheduler range `[0, 100]`. `framework.MinNodeScore` is reserved for
invalid or missing metadata. Valid nodes are normalized into the range
`[framework.MinNodeScore + 1, framework.MaxNodeScore]`, so a valid node at the
lower end of the min-max range is still distinguishable from a node whose
metadata could not be scored.

If all valid candidate nodes have the same raw score, the plugin assigns
`framework.MinNodeScore + 1` to those valid candidates and
`framework.MinNodeScore` to invalid candidates. If every candidate is valid and
has the same raw score, every candidate receives `framework.MinNodeScore + 1`.
In that case, the plugin becomes neutral among those valid nodes and leaves the
final placement decision to other scheduler plugins and tie-breaking behavior.

This also means that if all candidates are missing the configured metadata, the
plugin does not fail scheduling; it simply stops influencing the ranking.

### Validation and defaults

The proposal includes configuration validation with the following checks:

- `metadataKey` must not be empty
- `metadataSource` must be `Label` or `Annotation`
- `metadataType` must be `Number` or `Timestamp`
- `scoringStrategy` must be one of the supported enum values
- numeric metadata must use `Highest` or `Lowest`
- timestamp metadata must use `Newest` or `Oldest`

For the v1 config API, `timestampFormat` defaults to RFC3339 when
`metadataType` is `Timestamp` and the field is omitted.

## Known limitations

1. The plugin evaluates exactly one metadata key per configuration.
2. Metadata values are strings on the `Node` object, so typing and freshness are
   external operational concerns.
3. The plugin does not distinguish between intentionally absent metadata and
   malformed metadata; both result in the lowest effective influence.
4. The invalid metadata sentinel uses `math.MinInt64`. If a configured metadata
   value is converted to `math.MinInt64`, the plugin treats it as invalid or
   absent metadata rather than as a valid score.
5. Numeric metadata is accepted as `float64`, but the scheduler framework uses
   `int64` scores, so numeric values are rounded before scoring. This can cause
   small precision differences for fractional values; use integer metadata
   values when exact ordering matters.
6. The plugin is intentionally advisory. It cannot guarantee placement on nodes
   with a preferred metadata value.

## Test plan

The implementation is expected to ship with:

1. unit tests for
    - argument validation and type/strategy compatibility
    - unit tests for raw score calculation for numeric and timestamp metadata
    - unit tests for normalization behavior, including equal scores and missing metadata
2. integration tests that run the scheduler with only `NodeMetadata` scoring
   enabled and verify placement for:
    - highest numeric labels
    - lowest numeric annotations
    - newest timestamps
    - oldest timestamps
    - mixed nodes where some candidates are missing metadata
    - decimal numeric values
    - Unix epoch values represented as numbers

## Graduation criteria

- **Alpha**
  - Implemented and validate plugin config API
  - Unit tests and integration test from Test Plan.
  - Publish example manifests and plugin documentation.
- **Beta**
  - Add E2E tests.
  - Provide beta-level documentation.


## Production Readiness Review Questionnaire

### Feature enablement and rollback

**How can this feature be enabled or disabled in a live cluster?**

- Enable by registering the plugin in the scheduler binary and adding
  `NodeMetadata` to the `score.enabled` plugin set in the scheduler profile.
- Disable by removing `NodeMetadata` from the scheduler profile or setting its
  weight to zero through configuration updates.

**Does enabling the feature change any default behavior?**

No. The feature is opt-in and affects only scheduler profiles that explicitly
enable it.

**Can the feature be disabled without downtime?**

It requires a scheduler configuration rollout consistent with normal scheduler
operations. It does not require node reprovisioning.

The plugin is optional and enabled only through scheduler configuration.
Upgrading requires adding the plugin binary support and configuration.
Downgrading requires removing the plugin from the scheduler profile.

If the plugin is disabled, scheduling falls back to the remaining configured
plugins with no migration or data conversion step.

### Scalability

The plugin performs constant-time work per node candidate:

- one label or annotation lookup
- one parse step
- one raw score calculation

No additional informers, caches, or remote calls are required in the initial
design. The per-node overhead is limited to one lookup, one parse step, and one
raw score calculation.

### Troubleshooting

Operators should verify:

1. the configured key exists on candidate nodes
2. values are encoded in the expected format
3. the scheduler profile enables the plugin with the intended weight
4. timestamp layouts match the actual string representation on nodes

Because invalid or missing metadata becomes a low score instead of a scheduling
error, troubleshooting should include inspecting node metadata directly when the
observed ranking is weaker than expected.

## Implementation History

- 2025-12-12: PR #942 proposed the `NodeMetadata` scoring plugin together with
  config API support, validation, examples, documentation, unit tests, and
  integration tests.
- 2026-05-14: this KEP was added to document the design and behavior.

## Alternatives

1. **Node affinity or taints/tolerations**
   These mechanisms are useful for hard placement constraints, but they do not
   provide a generic ranking function across arbitrary metadata values.
2. **Dedicated plugin per use case**
   A separate plugin for cost, maintenance, hardware generation, or age would
   duplicate the same core logic and increase maintenance burden.
3. **Multi-key expression engine**
   A richer expression model could support more advanced ranking, but it would
   add significant API and validation complexity. The initial design chooses a
   smaller surface area.

## References

- [PR #942: Add NodeMetadata scoring plugin](https://github.com/kubernetes-sigs/scheduler-plugins/pull/942)
- [NodeMetadata plugin README in the PR branch](https://github.com/criteo-forks/scheduler-plugins/blob/alik-master/pkg/nodemetadata/README.md)
