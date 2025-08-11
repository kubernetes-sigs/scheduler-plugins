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
