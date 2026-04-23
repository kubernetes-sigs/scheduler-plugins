# Energy Gradient Plugin

Scores nodes based on energy gradients from `/egs/v1/gradient`.

## Overview

The EnergyGradient plugin enables energy-aware scheduling by scoring nodes based on their executable marginal energy capacity exposed via EGS (Energy-Gradient Signaling).

## Node Configuration

Annotate nodes with their EGS endpoint:

```yaml
apiVersion: v1
kind: Node
metadata:
  name: node-1
  annotations:
    egs.ear-standard.org/endpoint: "http://egs-exporter:9100"
```

## Scoring

```
score = marginal_capacity_kw × (1 - cost_index) × (thermal_headroom_pct / 100)
```

Nodes with:
- Higher `marginal_capacity_kw` (more available ΔkW)
- Lower `instantaneous_cost_index` (cheaper)
- Higher `thermal_headroom_pct` (more thermal margin)

...receive higher scores and are preferred for scheduling.

## Configuration

Enable the plugin in the scheduler configuration:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: EnergyGradient
```

## Reference

- EGS Standard: https://ear-standard.org
- EGS Exporter: https://github.com/oerc-s/egs-exporter
