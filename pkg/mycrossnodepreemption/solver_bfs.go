package mycrossnodepreemption

import (
	"fmt"
	"math/rand"
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

// PlanFunc generates an ordered move list to free just-enough on `target` for `pending`.
// `trial` and `rng` let stochastic planners vary attempts; deterministic planners ignore them.
func bfsPlan(
	pending *SolverPod,
	target *SolverNode,
	nodes map[string]*SolverNode,
	order []*SolverNode,
	moveGate *int32,
	_ map[string]struct{},
	_ int,
	_ *rand.Rand,
) ([]MoveLite, bool) {
	finalDest, ok := bfsTryFreeTarget(pending, target, nodes, moveGate)
	if !ok {
		return nil, false
	}
	origPlacements := buildOrigPlacements(order)
	return createMoves(finalDest, origPlacements), true
}

// bfsTryFreeTarget runs a breadth-first search over move-count to satisfy all deficits.
// Initial deficit 0 is the root target (where the preemptor will land).
// Returns: coalesced finalDest + ordered moves if a plan exists within caps.
func bfsTryFreeTarget(
	pending *SolverPod,
	target *SolverNode,
	nodes map[string]*SolverNode,
	moveGate *int32,
) (finalDest map[string]string, ok bool) {

	rootNeedCPU := max64(0, pending.ReqCPUm-target.AllocCPUm)
	rootNeedMem := max64(0, pending.ReqMemBytes-target.AllocMemBytes)

	init := BfsState{
		Deficits:  []Deficit{{Node: target.Name, DeficitCPU: rootNeedCPU, DeficitMem: rootNeedMem}},
		NetDelta:  map[string]Delta{},
		FinalDest: map[string]string{},
	}

	front := []BfsState{init}
	visited := map[string]bool{bfsStateKey(init.Deficits, init.FinalDest): true}

	for depth := 0; depth <= SolverBfsMaxDepth; depth++ {
		for _, st := range front {
			if len(st.Deficits) == 0 {
				return st.FinalDest, true
			}
		}
		if depth == SolverBfsMaxDepth {
			break
		}

		next := make([]BfsState, 0, len(front)*4)
		for _, state := range front {
			if len(state.Deficits) == 0 {
				continue
			}
			need := state.Deficits[0]

			victims := getVictims(nodes[need.Node], VictimOptions{
				Strategy: VictimsBFS,
				MoveGate: moveGate,
				NeedCPU:  need.DeficitCPU,
				NeedMem:  need.DeficitMem,
				Cap:      SolverBfsMaxVictimsPerNode,
			})
			if len(victims) == 0 {
				continue
			}

			destinations := destsByFree(nodes, SolverBfsMaxDestsPerLevel)

			for _, v := range victims {
				if state.FinalDest[v.UID] != "" {
					continue
				}
				for _, destNode := range destinations {
					if destNode.Name == need.Node {
						continue
					}
					if destNode.Pods[v.UID] != nil {
						continue
					}
					if destNode.Name == target.Name { // <- fixed (was rootTarget)
						continue
					}

					dstAdj := state.NetDelta[destNode.Name]
					needCPU2 := max64(0, v.ReqCPUm-(destNode.AllocCPUm+dstAdj.CPU))
					needMem2 := max64(0, v.ReqMemBytes-(destNode.AllocMemBytes+dstAdj.Mem))

					freedCPU := min64(v.ReqCPUm, need.DeficitCPU)
					freedMem := min64(v.ReqMemBytes, need.DeficitMem)
					if needCPU2 > freedCPU || needMem2 > freedMem {
						continue
					}

					succ := BfsState{
						Deficits:  cloneDeficits(state.Deficits),
						NetDelta:  cloneNetDelta(state.NetDelta),
						FinalDest: cloneFinalDest(state.FinalDest),
					}

					addNetDelta(succ.NetDelta, need.Node, +v.ReqCPUm, +v.ReqMemBytes)
					addNetDelta(succ.NetDelta, destNode.Name, -v.ReqCPUm, -v.ReqMemBytes)

					remCPU := max64(0, need.DeficitCPU-v.ReqCPUm)
					remMem := max64(0, need.DeficitMem-v.ReqMemBytes)
					if remCPU == 0 && remMem == 0 {
						succ.Deficits = succ.Deficits[1:]
					} else {
						succ.Deficits[0].DeficitCPU = remCPU
						succ.Deficits[0].DeficitMem = remMem
					}

					if needCPU2 > 0 || needMem2 > 0 {
						succ.Deficits = append(succ.Deficits, Deficit{
							Node: destNode.Name, DeficitCPU: needCPU2, DeficitMem: needMem2,
						})
					}

					succ.FinalDest[v.UID] = destNode.Name

					key := bfsStateKey(succ.Deficits, succ.FinalDest)
					if visited[key] {
						continue
					}
					visited[key] = true
					next = append(next, succ)
				}
			}
		}
		front = next
	}
	return nil, false
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

// Deficit represents a CPU/Mem deficit to free on a node.
type Deficit struct {
	// Name of the node with this deficit
	Node string
	// CPU deficit to free on this node
	DeficitCPU int64
	// Mem deficit to free on this node
	DeficitMem int64
}

// BfsState is a node in the BFS search tree.
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
