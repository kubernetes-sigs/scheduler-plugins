# Overview

This folder holds some sample plugin implementations based on [QoS
(Quality of Service) class](https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/)
of Pods.

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [x] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## QOS QueueSort Plugin

Sort pods by .spec.priority and breaks ties by the [quality of service class](https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/#qos-classes).
Specifically, this plugin enqueue the Pods with the following order:

- Guaranteed (requests == limits)
- Burstable (requests < limits)
- BestEffort (requests and limits not set)
