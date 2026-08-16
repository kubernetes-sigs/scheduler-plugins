# NodeMetadata Scheduler Plugin

The NodeMetadata plugin scores nodes based on their metadata (labels or annotations) containing numeric values or timestamps.

## Overview

This plugin allows you to influence scheduling decisions based on custom node metadata. It supports two types of metadata:

1. **Numeric values** - Score nodes based on numeric labels/annotations
2. **Timestamps** - Score nodes based on timestamp labels/annotations

## Configuration

### Plugin Arguments

The plugin accepts the following configuration parameters:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `metadataKey` | string | Yes | The name of the label or annotation to use for scoring |
| `metadataSource` | string | Yes | Where to read metadata from: `"Label"` or `"Annotation"` |
| `metadataType` | string | Yes | Type of metadata value: `"Number"` or `"Timestamp"` |
| `scoringStrategy` | string | Yes | How to score nodes (see below) |
| `timestampFormat` | string | Conditional | Go time format string (required when `metadataType` is `"Timestamp"`) |

### Scoring Strategies

#### For Numeric Metadata (`metadataType: "Number"`)

- **`Highest`** - Nodes with higher numeric values get higher scores
- **`Lowest`** - Nodes with lower numeric values get higher scores

#### For Timestamp Metadata (`metadataType: "Timestamp"`)

- **`Newest`** - Nodes with newer (more recent) timestamps get higher scores
- **`Oldest`** - Nodes with older timestamps get higher scores

## Usage Examples

### Example 1: Score by Node Priority (Numeric Label)

Prefer nodes with higher priority values:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeMetadata
    pluginConfig:
      - name: NodeMetadata
        args:
          metadataKey: "node.example.com/priority"
          metadataSource: "Label"
          metadataType: "Number"
          scoringStrategy: "Highest"
```

Node labels:
```yaml
apiVersion: v1
kind: Node
metadata:
  name: node1
  labels:
    node.example.com/priority: "100"
---
apiVersion: v1
kind: Node
metadata:
  name: node2
  labels:
    node.example.com/priority: "50"
```

Result: `node1` gets a higher score than `node2`.

### Example 2: Score by Cost (Numeric Annotation, Lowest Wins)

Prefer nodes with lower cost:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeMetadata
    pluginConfig:
      - name: NodeMetadata
        args:
          metadataKey: "node.example.com/hourly-cost"
          metadataSource: "Annotation"
          metadataType: "Number"
          scoringStrategy: "Lowest"
```

Node annotations:
```yaml
apiVersion: v1
kind: Node
metadata:
  name: node1
  annotations:
    node.example.com/hourly-cost: "2.5"
---
apiVersion: v1
kind: Node
metadata:
  name: node2
  annotations:
    node.example.com/hourly-cost: "1.2"
```

Result: `node2` gets a higher score than `node1`.

### Example 3: Score by Last Update Time (Newest Timestamp)

Prefer nodes that were updated most recently:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeMetadata
    pluginConfig:
      - name: NodeMetadata
        args:
          metadataKey: "node.example.com/last-maintenance"
          metadataSource: "Annotation"
          metadataType: "Timestamp"
          scoringStrategy: "Newest"
          timestampFormat: "2006-01-02T15:04:05Z07:00"  # RFC3339
```

Node annotations:
```yaml
apiVersion: v1
kind: Node
metadata:
  name: node1
  annotations:
    node.example.com/last-maintenance: "2025-12-01T10:00:00Z"
---
apiVersion: v1
kind: Node
metadata:
  name: node2
  annotations:
    node.example.com/last-maintenance: "2025-12-03T10:00:00Z"
```

Result: `node2` gets a higher score than `node1` (more recently maintained).

### Example 4: Score by Node Age (Oldest Timestamp)

Prefer older, more stable nodes:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeMetadata
    pluginConfig:
      - name: NodeMetadata
        args:
          metadataKey: "node.example.com/provisioned-at"
          metadataSource: "Label"
          metadataType: "Timestamp"
          scoringStrategy: "Oldest"
          timestampFormat: "2006-01-02"
```

Node labels:
```yaml
apiVersion: v1
kind: Node
metadata:
  name: node1
  labels:
    node.example.com/provisioned-at: "2025-11-01"
---
apiVersion: v1
kind: Node
metadata:
  name: node2
  labels:
    node.example.com/provisioned-at: "2025-12-01"
```

Result: `node1` gets a higher score than `node2` (older node).

## Timestamp Formats

The `timestampFormat` field uses Go's time format syntax. Common formats:

| Format Name | Format String | Example |
|-------------|---------------|---------|
| RFC3339 | `"2006-01-02T15:04:05Z07:00"` | `"2025-12-04T10:30:00Z"` |
| Date only | `"2006-01-02"` | `"2025-12-04"` |
| Custom | `"2006-01-02 15:04:05"` | `"2025-12-04 10:30:00"` |
| Unix (use Number type instead) | N/A | `"1701691800"` |

For Unix timestamps (seconds since epoch), use `metadataType: "Number"` instead.

## Use Cases

1. **Cost Optimization** - Prefer cheaper nodes
2. **Hardware Generation** - Prefer newer or older hardware
3. **Maintenance Windows** - Avoid recently maintained or prefer well-maintained nodes
4. **Custom Priorities** - Implement business-specific node prioritization
5. **Node Aging** - Distribute workloads based on node age
6. **Performance Tiers** - Score nodes by performance benchmarks

## Behavior Notes

- If a node doesn't have the specified metadata key, it receives a score of 0 (lowest)
- Scores are normalized to the range [0, 100] (framework.MinNodeScore to framework.MaxNodeScore)
- The plugin can be combined with other scoring plugins using weighted scores
- Invalid metadata values (e.g., unparseable timestamps) result in a score of 0 for that node

## Testing

Run the plugin tests:

```bash
cd pkg/nodemetadata
go test -v
```
