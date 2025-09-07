package mycrossnodepreemption

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

/*
Plan:

A. Worklist (pending pods order)
   - Priority:            DESC (higher first)
   - Size (CPU*MEM):      DESC
   - CPU:                 DESC
   - MEM:                 DESC
   - UID:                 ASC
   Rationale: big-first reduces fragmentation and later relocations.

B. For each pending pod p
   1) Quick feasibility (cluster slack)
      If sum(FreeCPU) < p.CPU or sum(FreeMEM) < p.MEM → go to §4 (Evict).
      Otherwise continue (relocations may still be needed).

   2) Direct fit (Best-Fit)
      Pick node with minimum post-waste:
         (freeCPU - p.CPU, then freeMEM - p.MEM; tie by node name)
      If it fits, place p and continue.

   3) Moves-only (no evictions)
      Goal: free “just enough” on one promising node with the FEWEST moves.
      3.1 Target nodes order
          For each node n compute deficits for p:
            defCPU = max(0, p.CPU - n.freeCPU)
            defMEM = max(0, p.MEM - n.freeMEM)
          Score(n) = max(defCPU/p.CPU, defMEM/p.MEM)  (ascending)
          Ties: (defCPU+defMEM) ASC → predicted post-placement waste ASC → name ASC
      3.2 Per-target planning (virtual)
          - Use a tiny *virtual delta*: a per-node scratchpad of (ΔCPU, ΔMEM) updated
            when tentatively moving/swapping pods. Fit checks use (free + delta).
          - Try up to maxTrialsPerPod *fresh* plans for this p on this target,
            each respecting a cap of maxMovesPerPod planned moves.
          - Within a plan, repeatedly pick victims from the target node:
              Preference order for victims:
                • alreadyMoved (seen earlier in this batch)
                • single-victim coverage of (defCPU, defMEM)
                • higher relocability (# of destination nodes where it fits under current delta)
                • smaller overshoot of needs (CPU/MEM)
                • lower priority
                • larger size (CPU*MEM), then smaller CPU, smaller MEM, UID ASC
              Try:
                (a) Direct move victim → its best-fit destination under current delta
                (b) Else, try a swap with some q on a destination node such that both nodes stay ≥ 0
              Update delta after each tentative step. Stop the plan as soon as the target
              node virtually fits p. If a step would exceed maxMovesPerPod, abort this plan.
          - Keep the plan with the FEWEST moves across targets (tiny jitter allowed for tie-breaks).
          - Optional early success: if after any tentative steps `batchFitOnResidual(delta, rest)`
            succeeds for the remaining pending pods, coalesce and commit immediately.
      3.3 Commit
          - Verify plan (endpoints exist; final free ≥ 0) and apply once via two-phase.
          - Place p on the chosen target. Record moves in `newPlacements` and add their UIDs to `alreadyMoved`.

   4) Evict (only when it guarantees improvement)
		- Policy:
		  1) Only candidates where evicting v alone enables placing p on v's node.
		  2) Among candidates, find the *minimum priority*.
	      3) Within that lowest-priority tier: prefer alreadyMoved; then size desc; CPU desc; MEM desc; node name asc; UID asc.

Mode guardrails
   - Batch mode:
       * Moves: no priority gate (still respect Protected).
       * Evictions: victim.priority < p.priority (strict).
   - Single-preemptor mode:
       * Moves allowed only if victim.priority ≤ preemptor.priority.
       * Evictions allowed only if victim.priority < preemptor.priority (strict).
*/

func runSolverSwap(in SolverInput) *SolverOutput {
	nodes, _, pending, order, pre := buildClusterState(in)

	if len(pending) == 0 {
		klog.InfoS("swap: nothing to place")
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	// A. Worklist
	worklist, singleMode, moveGatePtr := buildWorklist(pending, pre)

	// Batch accounting
	newPlacements := make(map[string]string)
	var evicts []Placement
	movedUIDs := make(map[string]struct{})

	stop := false
	var stopAt int32

	for _, p := range worklist {
		if stop && p.Priority <= stopAt {
			break
		}
		if singleMode && pre != nil && p.UID != pre.UID {
			continue
		}

		placed, infeasible := placeOnePodSwap(
			p, nodes, order,
			moveGatePtr,
			evictGateForPod(p, singleMode, pre),
			movedUIDs, newPlacements, &evicts,
		)

		if !placed {
			if singleMode || infeasible {
				return stableOutput("INFEASIBLE", newPlacements, evicts, in)
			}
			stop = true
			stopAt = p.Priority
			klog.InfoS("swap: stopping batch at priority; discarding remainder",
				"priority", stopAt, "uid", p.UID)
		}
	}

	return stableOutput("FEASIBLE", newPlacements, evicts, in)
}

// -----------------------------------------------------------------------------
// B. Place one pending pod
// -----------------------------------------------------------------------------

func placeOnePodSwap(
	p *pLite,
	nodes map[string]*nLite,
	order []*nLite,
	moveGate *int32,
	evictGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
	evicts *[]Placement,
) (bool, bool) {

	// 1) Cluster slack
	if !clusterHasSlack(order, p) {
		goto tryEvict
	}

	// 2) Direct best-fit
	if to, ok := bestDirectFit(order, p); ok {
		nodes[to].addPod(p)
		newPlacements[p.UID] = to
		return true, false
	}

	// 3) Moves-only (pick minimal plan across targets)
	if placeByMovesOnly(p, nodes, order, moveGate, movedUIDs, newPlacements) {
		return true, false
	}

	// 4) Evict (strictly lower prio, enabling-only)
tryEvict:
	v, on := pickLargestEnablingEviction(order, p, evictGate, movedUIDs)
	if v == nil || on == nil {
		return false, true
	}
	delete(newPlacements, v.UID)
	delete(movedUIDs, v.UID)
	on.removePod(v)
	*evicts = append(*evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})

	if on.canPodFit(p.CPUm, p.MemBytes) {
		on.addPod(p)
		newPlacements[p.UID] = on.Name
		return true, false
	}
	// Defensive fallback
	if to, ok := bestDirectFit(order, p); ok {
		nodes[to].addPod(p)
		newPlacements[p.UID] = to
		return true, false
	}
	return false, true
}

// -----------------------------------------------------------------------------
// B.3 Moves-only: choose target by smallest deficit; plan min moves; apply
// -----------------------------------------------------------------------------

type deltaDM struct{ cpu, mem int64 }

func placeByMovesOnly(
	p *pLite,
	nodes map[string]*nLite,
	order []*nLite,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) bool {
	targets := orderTargetsByDeficit(order, p)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	var bestMoves []moveLite
	bestTarget := ""
	bestCount := math.MaxInt32
	found := false

	for _, t := range targets {
		// Try a few fresh relocation attempts for this target node
		for trial := 0; trial < SolverSwapMaxTrialsPerPendingPod; trial++ {
			mvs, ok := planOnTargetVirtual(p, t, order, moveGate, movedUIDs, rng, SolverSwapMaxMovesForPendingPod)
			if ok && len(mvs) < bestCount {
				bestMoves = mvs
				bestTarget = t.Name
				bestCount = len(mvs)
				found = true
				if bestCount <= 1 { // can’t beat 0/1 anyway
					break
				}
			}
		}
		if found && bestCount <= 1 {
			break
		}
	}

	if !found {
		return false
	}

	// Commit chosen plan and place p
	all := buildAllPodsMap(order)
	if !verifyPlan(nodes, all, bestMoves) {
		return false
	}
	for _, mv := range bestMoves {
		newPlacements[mv.UID] = mv.To
		movedUIDs[mv.UID] = struct{}{}
	}
	if n := nodes[bestTarget]; n != nil && n.canPodFit(p.CPUm, p.MemBytes) {
		n.addPod(p)
		newPlacements[p.UID] = bestTarget
		return true
	}
	// defensive: best-fit in case of drift
	if to, ok := bestDirectFit(order, p); ok {
		nodes[to].addPod(p)
		newPlacements[p.UID] = to
		return true
	}
	return false
}

// --- Minimal virtual planner for one target node (no real mutations) ---

func planOnTargetVirtual(
	p *pLite,
	A *nLite,
	order []*nLite,
	moveGate *int32,
	movedUIDs map[string]struct{},
	rng *rand.Rand,
	capMoves int,
) ([]moveLite, bool) {
	needCPU := max64(0, p.CPUm-A.FreeCPUm)
	needMem := max64(0, p.MemBytes-A.FreeMemBytes)
	if needCPU == 0 && needMem == 0 {
		return nil, true
	}

	delta := map[string]deltaDM{}
	chosen := map[string]struct{}{}
	moves := make([]moveLite, 0, 8)

	victims := rankVictimsOnNode(A, needCPU, needMem, moveGate, movedUIDs, order)
	if len(victims) > 1 && rng.Intn(4) == 0 {
		i, j := rng.Intn(len(victims)), rng.Intn(len(victims))
		victims[i], victims[j] = victims[j], victims[i]
	}

	attempts := 0
	for _, v := range victims {
		if attempts >= SolverSwapMaxTrialsPerNode {
			break
		}
		if _, seen := chosen[v.UID]; seen {
			continue
		}

		// (1) direct move v -> D (honor cap)
		if D := bestDestUnderDelta(v, A.Name, order, delta); D != nil {
			if len(moves)+1 > capMoves {
				return nil, false // exceeded budget → let caller try another trial
			}
			moves = append(moves, moveLite{UID: v.UID, From: A.Name, To: D.Name})
			addDelta(delta, A.Name, D.Name, v.CPUm, v.MemBytes)
			needCPU = max64(0, needCPU-v.CPUm)
			needMem = max64(0, needMem-v.MemBytes)
			chosen[v.UID] = struct{}{}
			if needCPU == 0 && needMem == 0 {
				return moves, true
			}
			attempts++
			continue
		}

		// (2) swap v <-> q (counts as 2 moves, honor cap)
		if B, q := bestSwapUnderDelta(A, v, needCPU, needMem, order, delta, moveGate, movedUIDs, chosen); B != nil && q != nil {
			if len(moves)+2 > capMoves {
				return nil, false // swap would exceed budget → new trial
			}
			moves = append(moves,
				moveLite{UID: v.UID, From: A.Name, To: B.Name},
				moveLite{UID: q.UID, From: B.Name, To: A.Name},
			)
			da := delta[A.Name]
			db := delta[B.Name]
			da.cpu += v.CPUm - q.CPUm
			da.mem += v.MemBytes - q.MemBytes
			db.cpu += q.CPUm - v.CPUm
			db.mem += q.MemBytes - v.MemBytes
			delta[A.Name] = da
			delta[B.Name] = db

			// recompute remaining need for A under delta
			needCPU = max64(0, p.CPUm-(A.FreeCPUm+delta[A.Name].cpu))
			needMem = max64(0, p.MemBytes-(A.FreeMemBytes+delta[A.Name].mem))

			chosen[v.UID] = struct{}{}
			chosen[q.UID] = struct{}{}
			if needCPU == 0 && needMem == 0 {
				return moves, true
			}
			attempts++
			continue
		}

		attempts++
	}

	return nil, false
}

func addDelta(delta map[string]deltaDM, from, to string, cpu, mem int64) {
	da := delta[from]
	da.cpu += cpu
	da.mem += mem
	delta[from] = da
	db := delta[to]
	db.cpu -= cpu
	db.mem -= mem
	delta[to] = db
}

func bestDestUnderDelta(q *pLite, src string, order []*nLite, delta map[string]deltaDM) *nLite {
	var best *nLite
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.Name == src {
			continue
		}
		d := delta[n.Name]
		if (n.FreeCPUm+d.cpu) >= q.CPUm && (n.FreeMemBytes+d.mem) >= q.MemBytes {
			cw := (n.FreeCPUm + d.cpu) - q.CPUm
			mw := (n.FreeMemBytes + d.mem) - q.MemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && (best == nil || n.Name < best.Name)))) {
				best = n
				bestCPUWaste, bestMEMWaste = cw, mw
			}
		}
	}
	return best
}

func bestSwapUnderDelta(
	A *nLite,
	v *pLite,
	needCPU, needMem int64,
	order []*nLite,
	delta map[string]deltaDM,
	moveGate *int32,
	movedUIDs map[string]struct{},
	chosen map[string]struct{},
) (*nLite, *pLite) {
	var bestB *nLite
	var bestQ *pLite
	bestGain := int64(math.MinInt64)

	for _, B := range order {
		if B.Name == A.Name {
			continue
		}
		db := delta[B.Name]
		for _, q := range B.Pods {
			if !canMove(q, moveGate) {
				continue
			}
			if _, used := chosen[q.UID]; used {
				continue
			}
			finalA_CPU := (A.FreeCPUm + delta[A.Name].cpu) + v.CPUm - q.CPUm
			finalA_MEM := (A.FreeMemBytes + delta[A.Name].mem) + v.MemBytes - q.MemBytes
			finalB_CPU := (B.FreeCPUm + db.cpu) + q.CPUm - v.CPUm
			finalB_MEM := (B.FreeMemBytes + db.mem) + q.MemBytes - v.MemBytes
			if finalA_CPU < 0 || finalA_MEM < 0 || finalB_CPU < 0 || finalB_MEM < 0 {
				continue
			}
			// Limit gains by the remaining need on A
			gainCPU := min64(v.CPUm, needCPU) - min64(q.CPUm, needCPU)
			gainMEM := min64(v.MemBytes, needMem) - min64(q.MemBytes, needMem)
			gain := gainCPU + gainMEM

			mvQ := hasKey(movedUIDs, q.UID)
			mvBest := bestQ != nil && hasKey(movedUIDs, bestQ.UID)

			if bestQ == nil || gain > bestGain || (gain == bestGain && mvQ && !mvBest) {
				bestB, bestQ, bestGain = B, q, gain
			}
		}
	}
	return bestB, bestQ
}

// Utility to rebuild pod map from current nodes (for applyTwoPhase).
func buildAllPodsMap(order []*nLite) map[string]*pLite {
	m := make(map[string]*pLite, 256)
	for _, n := range order {
		for _, p := range n.Pods {
			m[p.UID] = p
		}
	}
	return m
}

// -----------------------------------------------------------------------------
// Victim ranking (reused, compact)
// -----------------------------------------------------------------------------

func rankVictimsOnNode(
	A *nLite,
	needCPU, needMem int64,
	moveGate *int32,
	movedUIDs map[string]struct{},
	order []*nLite,
) []*pLite {
	cands := make([]*pLite, 0, len(A.Pods))
	for _, q := range A.Pods {
		if !canMove(q, moveGate) {
			continue
		}
		cands = append(cands, q)
	}
	if len(cands) == 0 {
		return nil
	}

	type scored struct {
		q           *pLite
		singleCover bool
		relocCount  int
		overshoot   int64
		alreadyMv   bool
	}
	sc := make([]scored, 0, len(cands))
	for _, q := range cands {
		_, already := movedUIDs[q.UID]
		single := (q.CPUm >= needCPU && q.MemBytes >= needMem)
		ov := max64(0, q.CPUm-needCPU) + max64(0, q.MemBytes-needMem)
		rc := 0
		for _, n := range order {
			if n.Name == A.Name {
				continue
			}
			if n.canPodFit(q.CPUm, q.MemBytes) {
				rc++
			}
		}
		sc = append(sc, scored{q: q, singleCover: single, relocCount: rc, overshoot: ov, alreadyMv: already})
	}
	sort.Slice(sc, func(i, j int) bool {
		if sc[i].alreadyMv != sc[j].alreadyMv {
			return sc[i].alreadyMv
		}
		if sc[i].singleCover != sc[j].singleCover {
			return sc[i].singleCover
		}
		if sc[i].relocCount != sc[j].relocCount {
			return sc[i].relocCount > sc[j].relocCount
		}
		if sc[i].overshoot != sc[j].overshoot {
			return sc[i].overshoot < sc[j].overshoot
		}
		qi, qj := sc[i].q, sc[j].q
		if qi.Priority != qj.Priority {
			return qi.Priority < qj.Priority
		}
		si, sj := qi.CPUm*qi.MemBytes, qj.CPUm*qj.MemBytes
		if si != sj {
			return si > sj
		}
		if qi.CPUm != qj.CPUm {
			return qi.CPUm < qj.CPUm
		}
		if qi.MemBytes != qj.MemBytes {
			return qi.MemBytes < qj.MemBytes
		}
		return qi.UID < qj.UID
	})
	limit := SolverSwapMaxVictimsPerNode
	if limit > 0 && len(sc) > limit {
		sc = sc[:limit]
	}
	out := make([]*pLite, 0, len(sc))
	for _, s := range sc {
		out = append(out, s.q)
	}
	return out
}

// -----------------------------------------------------------------------------
// Gates (same semantics as before)
// -----------------------------------------------------------------------------

func canMove(p *pLite, gate *int32) bool {
	if p == nil || p.Node == "" || p.Protected {
		return false
	}
	if gate == nil {
		return true
	}
	return p.Priority <= *gate
}
