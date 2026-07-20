---
title: High Load Filter
weight: 6
---

# High Load Filter

`HighLoadFilter` is an optional `PreFilter` and `Filter` plugin that prevents
new Pods from being placed on nodes whose measured CPU or memory load is already
near a configured limit.

The plugin reads node utilization from
[load-watcher](https://github.com/paypal/load-watcher), captures one snapshot
for each scheduling cycle, and adds the incoming Pod's CPU and memory requests
before applying the configured thresholds. It is a standalone plugin and does
not require any Trimaran plugin to be enabled.

See the [plugin documentation](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/pkg/highloadfilter)
for configuration fields, examples, and failure behavior.
