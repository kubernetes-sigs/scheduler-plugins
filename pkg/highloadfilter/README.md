# HighLoadFilter

HighLoadFilter is a scheduler framework plugin that rejects nodes whose
predicted CPU or memory utilization exceeds configured limits. It uses
load-watcher metrics and runs independently of the Trimaran plugins.

The plugin implements `PreFilter` and `Filter`. `PreFilter` captures one metrics
snapshot for the scheduling cycle and calculates the incoming Pod's CPU and
memory requests. `Filter` calculates:

```text
predicted utilization = measured utilization + Pod request / node allocatable
```

A node is rejected when predicted utilization is greater than either configured
threshold. Equality is allowed.

## Configuration

HighLoadFilter can either connect to a separately deployed load-watcher service
or start load-watcher as an embedded library with a metrics provider. These
modes are mutually exclusive.

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

Embedded Prometheus provider:

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

When `metricProvider` is omitted, the embedded mode defaults to
`KubernetesMetricsServer`.

| Field | Default | Description |
|---|---:|---|
| `watcherAddress` | empty | Base URL of an external load-watcher service |
| `metricProvider.type` | `KubernetesMetricsServer` | Embedded backend: `KubernetesMetricsServer` or `Prometheus` |
| `metricProvider.address` | empty | Prometheus base URL |
| `metricProvider.token` | empty | Prometheus bearer token; protect scheduler configuration when used |
| `metricProvider.insecureSkipVerify` | `false` | Skip Prometheus TLS certificate verification |
| `usageThresholds.cpu` | `100` | Maximum predicted CPU utilization percentage |
| `usageThresholds.memory` | `100` | Maximum predicted memory utilization percentage |
| `failOpen` | `true` | Allow nodes when required metrics are missing or stale |
| `metricsUpdateIntervalSeconds` | `30` | Metrics cache refresh interval |
| `nodeMetricExpirationSeconds` | `180` | Maximum accepted snapshot age |

Thresholds must be in `[0, 100]`. The expiration interval must be at least as
large as the update interval.

## Failure behavior

With `failOpen: true`, a missing snapshot, stale snapshot, missing node, or
missing CPU/memory metric does not block scheduling. Metrics that are available
are still enforced. With `failOpen: false`, any required missing or stale metric
makes the node unschedulable.

The plugin returns `Unschedulable` for load and metrics failures because both
conditions can change without changing the Pod. Disabling the plugin requires
removing it from both the `preFilter` and `filter` extension points.

## Operational notes

- load-watcher and the metrics backend must use node names that match Kubernetes
  `Node.metadata.name`.
- Scheduler and metrics-source clocks should be synchronized because snapshot
  expiration uses the load-watcher Unix timestamp.
- Embedded load-watcher mode uses load-watcher's library lifecycle and HTTP
  listener. Use the service mode when multiple scheduler processes or plugins
  need an independently managed metrics pipeline.
- HighLoadFilter does not evict or reschedule Pods that are already running.
