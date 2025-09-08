package mycrossnodepreemption

import (
	"fmt"
	"sort"
)

/*
1) Build cluster state (nodes, pods, pending, order, preemptor)

2) Worklist (big-first)
   - Sort pending by: Priority DESC → size (CPU*MEM) DESC → CPU DESC → MEM DESC → UID ASC.
   - Single-preemptor mode: worklist has exactly the preemptor.
   - Batch mode: whole pending list is processed in order.

3) Per-pod placement (`placeOnePodBfs`)
   a) Cluster slack check (`clusterHasSlack`):
      If total free CPU/MEM across the cluster is insufficient for p, skip
      relocations and try the eviction path for p.
   b) Direct fit (`bestDirectFit`):
      If any node fits p, place it there (best-fit by post-waste).
   c) Relocations via BFS (`placeByBFS`):
      - Consider all nodes as targets, ordered by increasing deficit for p
        (`orderTargetsByDeficit`). For each target T, try to free just enough
        capacity on T with the FEWEST moves (no eviction).
      - For each T, run `bfsFreeTargets` up to `SolverBfsMaxDepth`.
        Keep the coalesced plan with the smallest number of moves across targets.
      - If a plan exists: verify + apply (two-phase), then place p on the
        chosen target.
   d) Eviction fallback (`pickLargestEnablingEviction`):
      Only consider a victim v if evicting v alone enables placing p on v’s node.
      Respect strict priority: victim.priority < gate (see “Mode & gates” below).
      Choose within the lowest-priority tier, preferring already-moved, then size.

4) Batch termination rule
   - If a pod at priority P cannot be placed without violating the rules,
     we stop the batch at that priority and return the partial result
     (same behavior as runSolverSwap). Single-preemptor mode returns INFEASIBLE.

BFS search (fewest-moves planner per target)
--------------------------------------------
We search by number of moves (layers = depth). The root represents the target
node T (where p will land) with an initial **deficit** equal to the shortfall
(needCPU, needMem) on T.

State (node in BFS tree):
  - deficits:  FIFO queue of outstanding (node, needCPU, needMem);
               the *front* is the currently active deficit node.
  - reserve:   per-node virtual deltas (ΔCPU, ΔMEM) that reflect tentative
               moves so far; fit checks use (free + reserve).
  - finalDest: map[UID]→dest, guaranteeing each UID is moved at most once.
  - moves:     ordered move list (each expansion adds exactly one move).

Edge (expansion step):
  - Pick a **victim** v from the *active* deficit node A (front of `deficits`),
    respecting: not Protected, priority ≤ prioLimit, and v not already moved.
  - Pick a **destination** D (by free); prune:
      • self-loop (D == A) or D already hosts v,
      • root-consumption (D == root target T), to keep T’s freed space intact.
  - Compute D’s shortage under current reserve (needCPU2, needMem2).
    **Progress rule:** do not create a worse deficit than the relief on A:
      needCPU2 ≤ min(v.CPUm, needCPU) AND needMem2 ≤ min(v.MemBytes, needMem).
    If violated, prune.
  - Produce successor:
      • `reserve[A] += (v.CPUm, v.MemBytes)`, `reserve[D] -= (v.CPUm, v.MemBytes)`
      • Decrease/resolve A’s deficit; if D is short, append a new deficit for D.
      • Record move (v: A→D) and `finalDest[v] = D`.

Layer goal:
  - A plan is found when `deficits` becomes empty (all shortfalls resolved).
    Coalesce `finalDest` against original locations to an (UID, from, to) list.
    The depth where the plan appears equals its number of moves.

Pruning & deduplication:
  - We dedupe with a signature `sig(deficits, finalDest, reserve)`:
      • deficits in order,
      • `finalDest` sorted by UID,
      • a compact snapshot of non-zero `reserve` entries.
  - Additional prunes: self-loop/root hit, progress rule, and revisit-avoidance.
  - BFS is bounded by:
      • `SolverBfsMaxDepth` (search layers),
      • `SolverBfsMaxVictimsPerNode` (victim fanout per active node),
      • `SolverBfsMaxDestsPerLevel` (destination fanout).
    Rough bound at depth k:  P(M,k) * min(D, N−2)^k  (see earlier discussion).

Mode & gates (same semantics as swap)
-------------------------------------
- Move gate:
    • Single-preemptor mode: only move pods with priority ≤ preemptor.priority
      (passed into BFS as `prioLimit`).
    • Batch mode: move gate disabled (allow any priority), still respect Protected.
- Evict gate:
    • Always strict: victim.priority < gate (preemptor.priority in single mode,
      p.priority in batch).

Commit path & safety
--------------------
- Candidates from BFS are **virtual**; before mutating:
    • `verifyCoalescedPlan` ensures all endpoints exist and final free ≥ 0
      (including the placement of p on the chosen target).
    • `applyTwoPhase` removes all moved pods from sources, then adds them to
      their destinations, preventing transient negatives.
- On success, we place p, update `newPlacements`, and remember moved UIDs.

Tuning knobs
------------
- `SolverBfsMaxDepth`               // max number of moves in a plan
- `SolverBfsMaxVictimsPerNode`      // breadth over victims per active node
- `SolverBfsMaxDestsPerLevel`       // breadth over candidate destinations
*/

func runSolverBfs(in SolverInput) *SolverOutput {
	return runSolverCommon(in,
		func(p *SolverPod, nodes map[string]*SolverNode, pods map[string]*SolverPod, order []*SolverNode,
			moveGate *int32, movedUIDs map[string]struct{}, newPlacements map[string]string,
		) bool {
			return placeByBfs(p, nodes, pods, order, moveGate, movedUIDs, newPlacements)
		},
		"bfs",
	)
}

// placeByBfs tries to place p by relocating existing pods via BFS.
// Returns true if successful (p placed, newPlacements and movedUIDs updated).
// On failure, the cluster state is unchanged.
func placeByBfs(
	pending *SolverPod,
	nodes map[string]*SolverNode,
	pods map[string]*SolverPod,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) (ok bool) {
	origPlacements := buildOrigPlacements(order)

	// Iterate over targets by increasing deficit for p
	// (zero-deficit targets first, i.e. direct-fit nodes)
	// Stop at the first target where a plan is found.
	bestMoves, bestTarget, ok := bestPlanAcrossTargets(pending, order, func(target *SolverNode) ([]MoveLite, bool) {
		needCPU := max64(0, pending.ReqCPUm-target.AllocCPUm)
		needMem := max64(0, pending.ReqMemBytes-target.AllocMemBytes)
		if needCPU == 0 && needMem == 0 {
			return nil, true // zero-move plan on t
		}
		// Try to free just enough on target via BFS
		// (fewest moves, respecting prioLimit)
		// Returns finalDest map if a plan is found within caps.
		finalDest, ok := bfsTryFreeTarget(nodes, target.Name, needCPU, needMem, moveGate)
		if !ok {
			return nil, false
		}
		// Make ordered moves from finalDest
		return createMoves(finalDest, origPlacements), true
	})
	if !ok {
		return false
	}

	// Commit the best plan found (verify + apply)
	return commitPlan(
		pending, bestTarget, bestMoves,
		nodes, pods, order,
		newPlacements, movedUIDs,
	)
}

// bfsTryFreeTarget runs a breadth-first search over move-count to satisfy all deficits.
// Initial deficit 0 is the root target (where the preemptor will land).
// Returns: coalesced finalDest + ordered moves if a plan exists within caps.
func bfsTryFreeTarget(
	nodes map[string]*SolverNode,
	rootTarget string,
	rootNeedCPU, rootNeedMem int64,
	moveGate *int32,
) (finalDest map[string]string, ok bool) {

	// BFS init state
	init := BfsState{
		Deficits:  []Deficit{{Node: rootTarget, DeficitCPU: rootNeedCPU, DeficitMem: rootNeedMem}},
		NetDelta:  map[string]Delta{},
		FinalDest: map[string]string{},
	}

	front := []BfsState{init}
	visited := map[string]bool{bfsStateKey(init.Deficits, init.FinalDest): true}

	// Layered BFS by depth (#moves)
	// (at most SolverBfsMaxDepth layers, each expanding all states in the layer)
	for depth := 0; depth <= SolverBfsMaxDepth; depth++ {
		// Goal check
		for _, st := range front {
			if len(st.Deficits) == 0 {
				return st.FinalDest, true
			}
		}
		if depth == SolverBfsMaxDepth {
			break
		}

		// Expand all states in the current layer (front)
		// (each expansion adds exactly one move, i.e. goes to the next layer)
		next := make([]BfsState, 0, len(front)*4)
		for _, state := range front {
			// Active deficit node
			if len(state.Deficits) == 0 {
				continue
			}
			need := state.Deficits[0]

			// Enumerate victims on active deficit node
			victims := getVictims(
				nodes[need.Node],
				moveGate,
				SolverBfsMaxVictimsPerNode,
				need.DeficitCPU,
				need.DeficitMem,
			)
			if len(victims) == 0 {
				continue
			}

			// Candidate destinations
			destinations := destsByFree(nodes, SolverBfsMaxDestsPerLevel)

			// Expand over victim/destination pairs
			for _, v := range victims {

				// Skip already-moved
				if state.FinalDest[v.UID] != "" {
					continue
				}
				// Try all candidate destinations and prune
				for _, destNode := range destinations {

					// Prune self-loop
					if destNode.Name == need.Node {
						continue
					}
					// Prune self-host
					if destNode.Pods[v.UID] != nil {
						continue
					}
					// Prune root-consumption
					if destNode.Name == rootTarget {
						continue
					}

					// Destination shortage under current reserve
					dstAdj := state.NetDelta[destNode.Name]
					needCPU2 := max64(0, v.ReqCPUm-(destNode.AllocCPUm+dstAdj.CPU))
					needMem2 := max64(0, v.ReqMemBytes-(destNode.AllocMemBytes+dstAdj.Mem))

					// Progress rule (don't make a worse deficit than we relieve)
					freedCPU := min64(v.ReqCPUm, need.DeficitCPU)
					freedMem := min64(v.ReqMemBytes, need.DeficitMem)
					if needCPU2 > freedCPU || needMem2 > freedMem {
						continue
					}

					// Successor state (adds exactly one move)
					successorState := BfsState{
						Deficits:  cloneDeficits(state.Deficits),
						NetDelta:  cloneNetDelta(state.NetDelta),
						FinalDest: cloneFinalDest(state.FinalDest),
					}

					// Reservations from this move
					addNetDelta(successorState.NetDelta, need.Node, +v.ReqCPUm, +v.ReqMemBytes)     // source
					addNetDelta(successorState.NetDelta, destNode.Name, -v.ReqCPUm, -v.ReqMemBytes) // destination

					// Update front deficit
					deficitCPU := max64(0, need.DeficitCPU-v.ReqCPUm)
					deficitMem := max64(0, need.DeficitMem-v.ReqMemBytes)
					// Check if resolved; pop front if so; else update
					if deficitCPU == 0 && deficitMem == 0 {
						successorState.Deficits = successorState.Deficits[1:]
					} else {
						successorState.Deficits[0].DeficitCPU = deficitCPU
						successorState.Deficits[0].DeficitMem = deficitMem
					}

					// Add new deficit for dest if needed
					if needCPU2 > 0 || needMem2 > 0 {
						successorState.Deficits = append(successorState.Deficits, Deficit{
							Node:       destNode.Name,
							DeficitCPU: needCPU2,
							DeficitMem: needMem2,
						})
					}
					// Record move
					successorState.FinalDest[v.UID] = destNode.Name

					key := bfsStateKey(successorState.Deficits, successorState.FinalDest)
					// Check if already visited
					if visited[key] {
						continue
					}
					visited[key] = true

					// Enqueue successor
					next = append(next, successorState)
				}
			}
		}
		front = next
	}

	// no plan found
	return nil, false
}

// getVictims returns candidate victims on node n, respecting:
// not Protected and (move gate == nil OR p.Priority <= *moveGate).
// Sorted by "most freeing", then prio (lower first), then size (smaller), then UID.
// If capK > 0, returns at most capK entries.
func getVictims(target *SolverNode, moveGate *int32, capK int, needCPU, needMem int64) []*SolverPod {
	victims := make([]*SolverPod, 0, len(target.Pods))
	for _, p := range target.Pods {
		if !podAllowedByPriority(p, moveGate, false) {
			continue
		}
		victims = append(victims, p)
	}

	// Normalize weights by current deficit (handles zeros)
	total := max64(1, needCPU) + max64(1, needMem)
	wCPU := float64(max64(1, needCPU)) / float64(total)
	wMem := 1.0 - wCPU

	sort.Slice(victims, func(i, j int) bool {
		// Contribution of each victim towards the remaining need
		contribCPU_i := float64(min64(victims[i].ReqCPUm, needCPU))
		contribCPU_j := float64(min64(victims[j].ReqCPUm, needCPU))
		contribMem_i := float64(min64(victims[i].ReqMemBytes, needMem))
		contribMem_j := float64(min64(victims[j].ReqMemBytes, needMem))

		// Weighted coverage scores
		score_i := wCPU*contribCPU_i + wMem*contribMem_i
		score_j := wCPU*contribCPU_j + wMem*contribMem_j

		if score_i != score_j {
			return score_i > score_j // higher coverage first
		}
		if victims[i].Priority != victims[j].Priority {
			return victims[i].Priority < victims[j].Priority
		}
		if victims[i].ReqCPUm != victims[j].ReqCPUm {
			return victims[i].ReqCPUm < victims[j].ReqCPUm
		}
		if victims[i].ReqMemBytes != victims[j].ReqMemBytes {
			return victims[i].ReqMemBytes < victims[j].ReqMemBytes
		}
		return victims[i].UID < victims[j].UID
	})
	if capK > 0 && capK < len(victims) {
		return victims[:capK]
	}
	return victims
}

// destsByFree returns nodes sorted by decreasing Alloc (i.e. increasing free).
// If capK > 0, returns at most capK entries.
func destsByFree(nodes map[string]*SolverNode, capK int) []*SolverNode {
	ns := make([]*SolverNode, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, n)
	}
	// Sort by decreasing CPU, then decreasing Mem, then Name
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].AllocCPUm != ns[j].AllocCPUm {
			return ns[i].AllocCPUm > ns[j].AllocCPUm
		}
		if ns[i].AllocMemBytes != ns[j].AllocMemBytes {
			return ns[i].AllocMemBytes > ns[j].AllocMemBytes
		}
		return ns[i].Name < ns[j].Name
	})
	// Cap if requested
	if capK > 0 && capK < len(ns) {
		return ns[:capK]
	}
	return ns
}

// bfsStateKey encodes a BFS state into a compact string for deduplication.
func bfsStateKey(defs []Deficit, fd map[string]string) string {
	b := make([]byte, 0, 256)

	// deficits in order (front is active)
	for _, d := range defs {
		b = append(b, d.Node...)
		b = append(b, '|')
		b = append(b, []byte(fmt.Sprintf("%d|%d;", d.DeficitCPU, d.DeficitMem))...)
	}

	// finalDest sorted for determinism
	uids := make([]string, 0, len(fd))
	for uid := range fd {
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	b = append(b, '#')
	for _, uid := range uids {
		b = append(b, uid...)
		b = append(b, '>')
		b = append(b, fd[uid]...)
		b = append(b, ';')
	}
	return string(b)
}

// createMoves coalesces finalDest map into an ordered move list,
// skipping no-ops and duplicates, sorted by UID.
func createMoves(finalDest map[string]string, orig map[string]string) (moves []MoveLite) {
	seen := map[string]bool{}
	uids := make([]string, 0, len(finalDest))
	for uid := range finalDest {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		to := finalDest[uid]
		from := orig[uid]
		if from == "" || to == "" || from == to {
			continue
		}
		if !seen[uid] {
			moves = append(moves, MoveLite{UID: uid, From: from, To: to})
			seen[uid] = true
		}
	}
	return moves
}

// addNetDelta adds (dcpu, dmem) to m[node], creating if needed.
func addNetDelta(m map[string]Delta, node string, deficitCPU, deficitMem int64) {
	addNodeDelta(m, node, deficitCPU, deficitMem)
}

// cloneNetDelta clones a reservation map.
func cloneNetDelta(reservation map[string]Delta) map[string]Delta {
	c := make(map[string]Delta, len(reservation))
	for key, value := range reservation {
		c[key] = value
	}
	return c
}

// cloneFinalDest clones a finalDest map.
func cloneFinalDest(finalDest map[string]string) map[string]string {
	c := make(map[string]string, len(finalDest))
	for key, value := range finalDest {
		c[key] = value
	}
	return c
}

// cloneDeficits clones a deficits slice.
func cloneDeficits(deficit []Deficit) []Deficit {
	c := make([]Deficit, len(deficit))
	copy(c, deficit)
	return c
}

type Deficit struct {
	// Name of the node with this deficit
	Node string
	// CPU deficit to free on this node
	DeficitCPU int64
	// Mem deficit to free on this node
	DeficitMem int64
}

type BfsState struct {
	// Outstanding nodes to free (front is active)
	Deficits []Deficit
	// Per-node net adjustment from tentative moves:
	//   sources:  +CPU/+Mem (freed)
	//   dests:    -CPU/-Mem (consumed)
	NetDelta map[string]Delta
	// Final chosen destination per UID (each UID moved at most once)
	FinalDest map[string]string
}
