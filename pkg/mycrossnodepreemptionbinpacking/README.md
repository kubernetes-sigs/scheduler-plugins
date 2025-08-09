# MyCrossNodePreemption Plugin

## Overview

An improved cross-node preemption plugin that addresses the limitations of the original CrossNodePreemption plugin. This plugin implements efficient algorithms for cross-node preemption with advanced optimization strategies.

## Test case

```bash
# Create cluster
./setup-cluster.sh mycluster 3

# Load plugins
./load-plugins.sh mycluster "MyCrossNodePreemption"

# Deploy test case
kubectl apply -f test-3node-scenario.yaml

# Verify deployment
kubectl get pods -o wide -n binpacking-test

# Deploy high-priority pod
kubectl apply -f test-high-priority-pod.yaml

# Verify preemption
kubectl get pods -o wide -n binpacking-test
```
