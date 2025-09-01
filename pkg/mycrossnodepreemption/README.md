# MyCrossNodePreemption Plugin

## Overview

An improved cross-node preemption plugin that addresses the limitations of the default scheduler's preemption. This plugin implements efficient algorithms for cross-node preemption with optimization strategies.


### Scheduling flow

TODO


## Open Questions

- What to do with evicted and blocked pods - put them to queue or try again immediately?
  - Jacopo: Fine, what i am doing now, by just letting them try again immediately
- What to with batched pods, we do not succeed to bind on first try?
- How to make large scale tests, and should I make a seperate test for the CP-SAT solver alone?
- Faster algorithm using simple heuristics if solver fails - which strategy to use - simply swapping?
- What to do with node-selectors, PDBs, and other rules.
  - Jacopo: Fine, to ignore these just write about it. May, the extra constaints actually will make the solver faster (smaller search space).

## TODOs General

- Write a proper README.md

## TODOs Test

- Test at which utilization the default scheduler stops to work properly.
- Large scale test on UCloud where i could set up multiple ubuntu servers each making on test.
- Test if python solver timing depends heavily on the node it is executed on (CPU type, etc.)
- Test the plugin works across workload type.
- Test CP-SAT vs. other solvers.

## TODOs Plugin

- Fix/test the digest issue.
- Write about that the OptimizeForEvery@PreEnqueue can not be deterministic as we do not determine which order the pods are taken. But maybe we should implemnt some logic to control it.
- Be careful when to take cluster stats
- Fast heuristic algorithm which runs in front of solver. So the solver needs to improve on that.
- Fix TODOs
- Make use of design patterns where possible.
- Cleanup code, structs and make the configmap more efficient

## Later TODOs

- Instead of having my own script for loading into kind, use the same method as done in Neri's repo, see his Makefile in root. Also, check his scheduler-config under manifests\optimizedpreemption 

## Write

- Write something about watchdogTTL
- Write something about the snapshotlister that it lags one scheduling cycle.
- Write about cache calls instead of client calls. Faster and better. https://pkg.go.dev/k8s.io/client-go/tools/cache
- Write about deletion-cost and that it is hard to evict the right workload-owned pods, therefore I found the new eviction API.
- Write about QueuingHints and that I end up using Pod Activator for reschedule queued pods.
- Write about atomics and we only use configmap for debugging.
- Write about Reserve/Unreserve and we use it for making sure pods gets scheduled to the node otherwise we can try again. We need this to ensure race conditions not happens. We cannot rely on snapshot alone.
- Write about that Optimizer is not deterministic, when having multiple workers. However, we need multiple workers, otherwise it is too slow.

## Developed

TODO

## Good commands

- Get pods

  ```bash
  kubectl get pods -o wide -n crossnode-test
  ```

- Getting saved solver plan from kube-scheduler

  ```bash
  kubectl -n kube-system get cm -l crossnode-plan
  kubectl -n kube-system get cm <CM> -o jsonpath='{.data.plan\.json}' | jq .
  ```

- Get reasons for pod(s) not scheduling

  ```bash
  kubectl --context kwok-kwok1 -n crossnode-test get events --field-selector involvedObject.kind=Pod -o json | jq '.items[] | {name: .involvedObject.name, reason: .reason, message: .message}'
  ```

- Build kwok-cluster with plugin and random pods

  ```bash
  docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .
  kwokctl create cluster --name kwok --config kwok-cluster.yaml
  ./fill-nodes-kwok.sh kwok 3 4 4
  ```

- Recreate cluster and fill nodes (continue if already deleted)

  ```bash
  kubectl delete ns crossnode-test >& /dev/null || true && ./fill-nodes-kwok.sh kwok 9 4 4 && ./cluster-usage.sh
  ```

  - Combined:

    ```bash
    docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile . && kwokctl create cluster --name kwok --config kwok-cluster.yaml && sleep 1 && ./fill-nodes-kwok.sh kwok 9 4 4 && ./cluster-usage.sh
    ```

## Install Metrics API in kind cluster

```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch -n kube-system deployment metrics-server --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

## Test

```bash
# Create cluster
./kind-create-cluster.sh mycluster 3

# Load plugins
./kind-load-plugins.sh mycluster "MyCrossNodePreemption"
```
