# MyCrossNodePreemption Plugin

## The cross-node preemption plan

The python script should provide the optimal pod placement plan as output, including all necessary moves and evictions. That means, it must consider the current state of the cluster, including resource availability and pod priorities, to generate a valid scheduling plan including the new pending pod. The optimization should prefer higher priorities first, that means it should place as many high priority pods first on the nodes as possible, then next priority level should be considered. The optimization is that it should place as many high priority pods as possible before considering lower priority ones, then it should try to minimize the number of evictions and moves required to achieve the desired state and minimize the number of nodes used.

The plugin will call this script in PostFilter with the current cluster state and the pending pod's requirements, and it will receive the proposed plan as output.

## Good commands

### Getting saved solver plan from kube-scheduler

kubectl -n kube-system get cm -l scheduler.x/crossnode-plan=true

kubectl -n kube-system get cm <CM> -o jsonpath='{.data.plan\.json}' | jq .

### Build kwok cluster with custom image and random pods

docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .

kwokctl create cluster --name kwok --config kwok-cluster.yaml

./fill-nodes-kwok.sh kwok 3 4 4

## TODOs

- Test more.
- Make solver depend on deployment and replicaset, that is if a pod belongs to a deployment or replicaset, the other pods in the same deployment/replicaset should also be evicted/deleted and so on.
- Consider to protect pods that have node-selectors and other rules
- Test that deleted controller-owned pods (ReplicaSets) aren't created after we have recreated them.
- Add a diff in python code, so only needed changes are sent to plugin.
- Check that the scheduler runs the plan correctly.
- Add a script to deploy many high priority pods.
- Maybe consider to not do a fully optimal placement only such so the pending pod can be scheduled. Otherwise, think about what it gives in the long run to optimize all nodes.
- Consider to evict lower priority pods first, instead of just evicting the lowest amount of pods.
- Simplify code and update readme.
- Demo: Next week.
- Test if python solver timing depends heavily on 

## Later

- Fix Neri's way of doing cross-node preemption by making several scheduling improvements. I think he uses Prefilters to only schedule the missing pods not scheduled yet in the stop-world timeframe. Actually, I think most of my code can be used for this case. the only difference is that we have to ensure that there is not coming any race conditions since other pods can be changed in the meantime. Note: Think about limitations of stop-the-world assumption. Actually, I don' think we use stop the world in my case, as it is done in one cycle, however doing the execution of plan, something of course could happen. Also I think it is fair to let the plugin run for some time since other pods to be scheduled also likely dont have space.
- Make my own heuristic based optimization plan.
- Instead of having my own script for loading into kind, use the same method as done in Neri's repo, see his Makefile in root. Also, check his scheduler-config under manifests\optimizedpreemption

## Optional improvements

- Only send needed changes from python to go.
- Consider to make a delay when evicting pods to avoid race conditions when they are recreated.
- Maybe different order of constraints should be considered.

## Testing plan

Paired replay (sequential) on a clean slate
- Run the same workload twice on a clean cluster (or after a full reset): once with default, once with yours.
- Fix all randomness: same pod count, priorities, affinities, creation timestamps, and random seed if your generator uses one.
- Advantages: isolates each scheduler; best for measuring packing efficiency and disruption without interference.

## Overview

An improved cross-node preemption plugin that addresses the limitations of the original CrossNodePreemption plugin. This plugin implements efficient algorithms for cross-node preemption with advanced optimization strategies.

## Developed

- setup-cluster.sh
- load-plugins.sh
- test-3node-scenario.yaml
- test-high-priority-pod.yaml
- Dockerfile for kube-scheduler
- python script to generate optimal scheduling plan

## Install Metrics API in kind cluster

```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch -n kube-system deployment metrics-server --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

## Kubectl commands

Read CPUs and memory capacity of the nodes:

```bash
kubectl get nodes -o jsonpath="{range .items[*]}{.metadata.name}{'\t'}{.status.capacity.cpu}{'\t'}{.status.capacity.memory}{'\n'}{end}"
```

Read CPUs and memory allocatable of the nodes:

```bash
kubectl get nodes -o jsonpath="{range .items[*]}{.metadata.name}{'\t'}{.status.allocatable.cpu}{'\t'}{.status.allocatable.memory}{'\n'}{end}"
```

## Test

```bash
# Create cluster
./setup-cluster.sh mycluster 3

# Load plugins
./load-plugins.sh mycluster "MyCrossNodePreemption"

# Deploy test case
kubectl apply -f test-3node-scenario.yaml

# Verify deployment
kubectl get pods -o wide -n crossnode-test

# Deploy high-priority pod
kubectl apply -f test-high-priority-pod.yaml

# Verify preemption
kubectl get pods -o wide -n crossnode-test
```

## Implementation Details

### Scheduling flow

Scheduler loop:

1. Take pending pod P from the queue.
2. Run PreFilter/Filter to find feasible nodes.
3. If any feasible → Score, Bind, done.
4. If none feasible → call PostFilter(P).
    - Your plugin computes a "rearrangement plan" (pod moves + possible evictions) to make space.
    - It executes that plan via the API (clone+pin moves, delete originals, evict victims).
    - It returns a "#"nominated node" for P.
5. P is retried shortly; caches have the new state; P fits on the nominated node.
6. Core scheduler binds P there.

### PostFilter

PostFilter is called after the Filter phase, but only when no feasible nodes were found for the pod P.

This plugin is responsible for computing a rearrangement plan to make space for a pod that could not be scheduled in the "normal" way because it requires resources that are not currently available on any node.

```kotlin
POSTFILTER(P):
  NODES ← snapshot of all worker nodes (capacity, usage, pods)
  CANDS ← BUILD_CANDIDATES(NODES, P)
  PLAN  ← FIND_BEST_PLAN(CANDS, P)
  if PLAN = none:
      return UNSCHEDULABLE
  EXECUTE_PLAN(PLAN)       // do moves (and if needed evictions)
  return NOMINATED_NODE for P
```

### Build candidates

```kotlin
BUILD_CANDIDATES(NODES, P):
  for each node N (skip control-plane):
    STATE   ← {allocatable, available, pods}
    VICTIMS ← pods on N with priority < priority(P)
    add candidate (node=N, state=STATE, victims=VICTIMS)
  return candidate list
```

### Choose the best target node (min evictions, then min moves)

```kotlin
FIND_BEST_PLAN(CANDIDATES, P):
  GLOBAL ← map node → STATE (for simulation)
  best ← none
  for each candidate C:
    deficitCPU  ← max(0, cpuReq(P)  - C.state.availableCPU)
    deficitMem  ← max(0, memReq(P)  - C.state.availableMem)

    PLAN ← PLAN_FOR_TARGET(P, target=C.node, deficits, GLOBAL)
    if PLAN better than best by:
         (fewer evictions) then (fewer moves):
        best ← PLAN

  return best
```

### Plan for a single target (move-then-evict with tiny search)

```kotlin
PLAN_FOR_TARGET(P, target T, deficits, GLOBAL):
  MOVABLE ← pods on T with priority < priority(P)
  sort MOVABLE by size descending (largest first)

  SIM ← deep copy of GLOBAL (for “what-if” simulation)
  PLAN ← empty {moves=[], evictions=[], target=T}
  freed ← {cpu=0, mem=0}
  movesLeft ← configurable budget

  for each pod X in MOVABLE while deficits not covered:
    if movesLeft = 0: break

    // try a simple move first
    D ← BEST_DESTINATION(X, SIM, exclude={T})

    // if simple move fails, try bounded DFS to free a destination for X
    if D = none and movesLeft > 1:
       (D, helperMoves) ← FREE_DEST_WITH_BOUNDED_SEARCH(X, P, T, SIM, depth=2, movesLeft-1)

    if D = none:
       mark X as "eviction candidate later"
       continue

    // apply helper moves + main move in SIM
    apply helperMoves in SIM; append to PLAN.moves; movesLeft -= len(helperMoves)
    move X→D in SIM; append to PLAN.moves; movesLeft--

    freed += resources(X)

    if freed covers deficits: break

  if freed still insufficient:
    pick from “eviction candidates” smallest-first until deficits are covered
    if still not covered → return none

  return PLAN
```

### Freeing a destination with bounded DFS + branch-and-bound (depth ≤ 2)

```kotlin
FREE_DEST_WITH_BOUNDED_SEARCH(RELOC=X, P, target T, SIM, depth, movesLeft):

  need ← resources(X)
  DESTS ← shortlist of K nodes (not T) ranked by smallest CPU gap to need

  for each dest D in DESTS:
    if D already fits need → return (D, [])

    bestPlan ← none
    DFS_FREE(D, depth, movesLeft, SIM, usedMoves=[], bestPlan)

    if bestPlan found → return (D, bestPlan)

  return (none, [])
```

### DFS with pruning (branch-and-bound)

```kotlin
DFS_FREE(D, depth, movesLeft, SIM, usedMoves, bestPlan):

  if D now fits the need:
     score ← (evictions=0, moves=len(usedMoves))
     if score better than bestPlan: bestPlan = copy(usedMoves)
     return

  if depth = 0 or movesLeft = 0: return

  // Upper bound (prune): even moving the largest 'movesLeft' eligible helpers
  // on D cannot cover the remaining gap → stop exploring this branch
  if UPPER_BOUND(D, movesLeft) < remaining gap: return

  HELPERS ← eligible helper pods on D (priority < P), sorted largest first

  for each helper H in HELPERS:
     // Option A: try direct move of H to some node not {D, T}
     D2 ← BEST_DESTINATION(H, SIM, exclude={D, T})
     if D2 exists:
        simulate move H→D2, recurse with movesLeft-1, then undo

     // Option B: one level deeper: free a destination for H
     if depth > 1 and movesLeft > 1:
        (D3, plan2) ← FREE_DEST_WITH_BOUNDED_SEARCH(H, P, T, SIM, depth-1, movesLeft-1)
        if D3 exists:
           simulate plan2 + move H→D3, recurse with movesLeft-1-len(plan2), then undo
```

### Destination choice heuristic (fast, stable)

```kotlin
BEST_DESTINATION(pod X, SIM, exclude):
  among schedulable nodes not in exclude that can fit X now,
  pick the node with the lowest CPU utilization after placing X
```

### Executing the plan (imperative side effects)

```kotlin
EXECUTE_PLAN(PLAN):
  // Moves first
  for each move (pod A: from S to D):
     create clone of A pinned to D (NodeName=D)
     delete original A

  small pause or poll until the target node shows expected free capacity

  // Then evictions (if any)
  for each victim V:
     delete V immediately

  // Return; scheduler will re-try P and bind on the nominated node
```

### Tiny glossary

- P: the pending, higher-priority pod we’re trying to place
- Target node (T): the node we try to make space on for P
- Move: relocate a lower-priority pod by cloning it pinned to a new node and deleting the original
- Eviction: delete a lower-priority pod (last resort)
- Bounded DFS: explore short chains of moves (depth ≤ 2) with pruning to avoid explosion
- Branch-and-bound: cut branches that cannot possibly free enough capacity (upper-bound test), and stop exploring once a strictly better plan isn’t possible.

