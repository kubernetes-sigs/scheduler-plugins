package mycrossnodepreemption

import (
	"fmt"
	"math"
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
		// BFS planner
		func(p *SolverPod, nodes map[string]*SolverNode, pods map[string]*SolverPod, order []*SolverNode,
			moveGate *int32, movedUIDs map[string]struct{}, newPlacements map[string]string,
		) bool {
			return placeByBFS(p, nodes, pods, order, moveGate, movedUIDs, newPlacements)
		},
		"bfs",
	)
}

// placeByBFS tries to place p by relocating existing pods via BFS.
// Returns true if successful (p placed, newPlacements and movedUIDs updated).
// On failure, the cluster state is unchanged.
func placeByBFS(
	p *SolverPod,
	nodes map[string]*SolverNode,
	pods map[string]*SolverPod,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) bool {
	orig := buildOrigMap(order)

	// Move gate policy
	prioLimit := int32(math.MaxInt32)
	if moveGate != nil {
		prioLimit = *moveGate
	}

	bestMoves, bestTarget, ok := bestPlanAcrossTargets(p, order, func(t *SolverNode) ([]moveLite, bool) {
		needCPU := max64(0, p.ReqCPUm-t.AllocCPUm)
		needMem := max64(0, p.ReqMemBytes-t.AllocMemBytes)
		if needCPU == 0 && needMem == 0 {
			return nil, true // zero-move plan on t
		}
		fd, ok := bfsFreeTargets(nodes, t.Name, needCPU, needMem, prioLimit)
		if !ok {
			return nil, false
		}
		return squashMoves(fd, orig), true
	})
	if !ok {
		return false
	}

	// Shared commit path
	return commitPlanAndPlace(
		p, bestTarget, bestMoves,
		nodes, pods, order,
		newPlacements, movedUIDs,
	)
}

// ========================= Helpers / scoring =========================

func eligibleVictimsSorted(n *SolverNode, prioLimit int32, capK int, needCPU, needMem int64) []*SolverPod {
	buf := make([]*SolverPod, 0, len(n.Pods))
	for _, p := range n.Pods {
		if p.Protected || p.Priority > prioLimit {
			continue
		}
		buf = append(buf, p)
	}
	// Decide weights by relative tightness. Add +1 to avoid 0 weight.
	wCPU := max64(1, needCPU)
	wMem := max64(1, needMem)
	// Normalize weights into {CPU:3, Mem:1} or {CPU:1, Mem:3} rough shape:
	if wCPU >= wMem {
		wCPU, wMem = 5, 1
	} else {
		wCPU, wMem = 1, 5
	}

	sort.Slice(buf, func(i, j int) bool {
		si := min64(buf[i].ReqCPUm, needCPU)*wCPU + min64(buf[i].ReqMemBytes, needMem)*wMem
		sj := min64(buf[j].ReqCPUm, needCPU)*wCPU + min64(buf[j].ReqMemBytes, needMem)*wMem
		if si != sj {
			return si > sj
		}
		if buf[i].Priority != buf[j].Priority {
			return buf[i].Priority < buf[j].Priority
		}
		if buf[i].ReqCPUm != buf[j].ReqCPUm {
			return buf[i].ReqCPUm < buf[j].ReqCPUm
		}
		if buf[i].ReqMemBytes != buf[j].ReqMemBytes {
			return buf[i].ReqMemBytes < buf[j].ReqMemBytes
		}
		return buf[i].UID < buf[j].UID
	})
	if capK > 0 && capK < len(buf) {
		return buf[:capK]
	}
	return buf
}

func destsByFree(nodes map[string]*SolverNode, capK int) []*SolverNode {
	ns := make([]*SolverNode, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].AllocCPUm != ns[j].AllocCPUm {
			return ns[i].AllocCPUm > ns[j].AllocCPUm
		}
		if ns[i].AllocMemBytes != ns[j].AllocMemBytes {
			return ns[i].AllocMemBytes > ns[j].AllocMemBytes
		}
		return ns[i].Name < ns[j].Name
	})
	if capK > 0 && capK < len(ns) {
		return ns[:capK]
	}
	return ns
}

func pushResv(m map[string]Delta, node string, dcpu, dmem int64) {
	addNodeDelta(m, node, dcpu, dmem)
}

func squashMoves(finalDest map[string]string, orig map[string]string) []moveLite {
	seen := map[string]bool{}
	out := make([]moveLite, 0, len(finalDest))
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
			out = append(out, moveLite{UID: uid, From: from, To: to})
			seen[uid] = true
		}
	}
	return out
}

// ============================ small utils ============================

// ----- Pure BFS state for freeing capacity on a set of nodes -----

type deficit struct {
	node    string
	needCPU int64
	needMem int64
}

type bfsState struct {
	deficits  []deficit         // outstanding nodes to free (front is active)
	reserve   map[string]Delta  // node -> reserved delta (+free on sources, -consumed on dests)
	finalDest map[string]string // UID -> final destination node (each UID moved at most once)
}

// helpers to clone small maps/slices (kept tiny by caps)
func cloneResv(m map[string]Delta) map[string]Delta {
	c := make(map[string]Delta, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
func cloneFD(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
func cloneDeficits(d []deficit) []deficit {
	c := make([]deficit, len(d))
	copy(c, d)
	return c
}

// bfsFreeTargets runs a breadth-first search over move-count to satisfy all deficits.
// Initial deficit 0 is the root target (where the preemptor will land).
// Returns: coalesced finalDest + ordered moves if a plan exists within caps.
func bfsFreeTargets(
	nodes map[string]*SolverNode,
	rootTarget string,
	rootNeedCPU, rootNeedMem int64,
	prioLimit int32,
) (map[string]string, bool) {

	init := bfsState{
		deficits:  []deficit{{node: rootTarget, needCPU: rootNeedCPU, needMem: rootNeedMem}},
		reserve:   map[string]Delta{},
		finalDest: map[string]string{},
	}

	front := []bfsState{init}
	initKey := sig(init.deficits, init.finalDest, init.reserve)
	visited := map[string]bool{initKey: true}
	maxFrontier := 0

	for depth := 0; depth <= SolverBfsMaxDepth; depth++ {
		// goal check within this layer
		for _, st := range front {
			if len(st.deficits) == 0 {
				return st.finalDest, true
			}
		}
		if depth == SolverBfsMaxDepth {
			break
		}

		next := make([]bfsState, 0, len(front)*4)
		for _, st := range front {
			if len(st.deficits) == 0 {
				continue
			}
			need := st.deficits[0]

			// enumerate victims on active deficit node
			vics := eligibleVictimsSorted(
				nodes[need.node],
				prioLimit,
				SolverBfsMaxVictimsPerNode,
				need.needCPU,
				need.needMem,
			)
			if len(vics) == 0 {
				continue
			}

			// candidate destinations
			dests := destsByFree(nodes, SolverBfsMaxDestsPerLevel)

			for _, v := range vics {
				if st.finalDest[v.UID] != "" {
					continue // each UID at most once
				}
				for _, dn := range dests {
					if dn.Name == need.node {
						continue
					}
					if dn.Pods[v.UID] != nil {
						continue
					}
					if dn.Name == rootTarget {
						continue // don't consume root freed space
					}
					// (If you keep a MaxTotalMoves cap, check it here and bump prunedCapMoves)

					// destination shortage under current reserve
					dstRes := st.reserve[dn.Name]
					needCPU2 := max64(0, v.ReqCPUm-(dn.AllocCPUm+dstRes.cpu))
					needMem2 := max64(0, v.ReqMemBytes-(dn.AllocMemBytes+dstRes.mem))

					// progress rule (don't make a worse deficit than we relieve)
					freedCPU := min64(v.ReqCPUm, need.needCPU)
					freedMem := min64(v.ReqMemBytes, need.needMem)
					if needCPU2 > freedCPU || needMem2 > freedMem {
						continue
					}

					// successor state (adds exactly one move)
					ns := bfsState{
						deficits:  cloneDeficits(st.deficits),
						reserve:   cloneResv(st.reserve),
						finalDest: cloneFD(st.finalDest),
					}

					// reservations from this move
					pushResv(ns.reserve, need.node, +v.ReqCPUm, +v.ReqMemBytes)
					pushResv(ns.reserve, dn.Name, -v.ReqCPUm, -v.ReqMemBytes)

					// reduce front deficit
					remCPU := max64(0, need.needCPU-v.ReqCPUm)
					remMem := max64(0, need.needMem-v.ReqMemBytes)
					if remCPU == 0 && remMem == 0 {
						ns.deficits = ns.deficits[1:]
					} else {
						ns.deficits[0].needCPU = remCPU
						ns.deficits[0].needMem = remMem
					}

					// add new deficit for dest if needed
					if needCPU2 > 0 || needMem2 > 0 {
						ns.deficits = append(ns.deficits, deficit{
							node:    dn.Name,
							needCPU: needCPU2,
							needMem: needMem2,
						})
					}
					// record move
					ns.finalDest[v.UID] = dn.Name

					key := sig(ns.deficits, ns.finalDest, ns.reserve)
					if visited[key] {
						continue
					}
					visited[key] = true
					next = append(next, ns)
				}
			}
		}

		// update aggregates for the layer
		if len(next) > maxFrontier {
			maxFrontier = len(next)
		}

		front = next
	}

	// no plan found
	return nil, false
}

// add this helper
func sigResv(r map[string]Delta) string {
	if len(r) == 0 {
		return "-"
	}
	type item struct {
		n    string
		c, m int64
	}
	arr := make([]item, 0, len(r))
	for k, v := range r {
		if v.cpu == 0 && v.mem == 0 {
			continue
		}
		arr = append(arr, item{n: k, c: v.cpu, m: v.mem})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].n < arr[j].n })
	if len(arr) == 0 {
		return "-"
	}
	b := make([]byte, 0, len(arr)*24)
	for _, it := range arr {
		b = append(b, it.n...)
		b = append(b, ':')
		b = append(b, []byte(fmt.Sprintf("%d,%d;", it.c, it.m))...)
	}
	return string(b)
}

// change sig to include reserve:
func sig(defs []deficit, fd map[string]string, res map[string]Delta) string {
	b := make([]byte, 0, 256)
	// deficits in order
	for _, d := range defs {
		b = append(b, d.node...)
		b = append(b, '|')
		b = append(b, []byte(fmt.Sprintf("%d|%d;", d.needCPU, d.needMem))...)
	}
	// finalDest sorted
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
	// reserve snapshot
	b = append(b, '|')
	b = append(b, sigResv(res)...)
	return string(b)
}
