package mycrossnodepreemption

import (
	"math"
	"math/rand"
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

 1. Quick feasibility (cluster slack)
    If sum(FreeCPU) < p.CPU or sum(FreeMEM) < p.MEM → go to §4 (Evict).
    Otherwise continue (relocations may still be needed).

 2. Direct fit (Best-Fit)
    Pick node with minimum post-waste:
    (freeCPU - p.CPU, then freeMEM - p.MEM; tie by node name)
    If it fits, place p and continue.

 3. Moves-only (no evictions)
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

 4. Evict (only when it guarantees improvement)
    - Policy:

 1. Only candidates where evicting v alone enables placing p on v's node.

 2. Among candidates, find the *minimum priority*.

 3. Within that lowest-priority tier: prefer alreadyMoved; then size desc; CPU desc; MEM desc; node name asc; UID asc.

Mode guardrails
  - Batch mode:
  - Moves: no priority gate (still respect Protected).
  - Evictions: victim.priority < p.priority (strict).
  - Single-preemptor mode:
  - Moves allowed only if victim.priority ≤ preemptor.priority.
  - Evictions allowed only if victim.priority < preemptor.priority (strict).
*/

func localSearchPlan(
	pending *SolverPod,
	target *SolverNode,
	nodes map[string]*SolverNode,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	trial int,
	rng *rand.Rand,
) ([]MoveLite, bool) {
	return localSearchTryFreeTarget(
		pending, target, order, moveGate, movedUIDs,
		rng, SolverLocalSearchMaxMovesPerPlan,
	)
}

func localSearchTryFreeTarget(
	pending *SolverPod,
	target *SolverNode,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	rng *rand.Rand,
	capMoves int,
) ([]MoveLite, bool) {
	needCPU := max64(0, pending.ReqCPUm-target.AllocCPUm)
	needMem := max64(0, pending.ReqMemBytes-target.AllocMemBytes)
	if needCPU == 0 && needMem == 0 {
		return nil, true
	}

	delta := map[string]Delta{}
	chosen := map[string]struct{}{}
	moves := make([]MoveLite, 0, 8)

	victims := getVictims(target, VictimOptions{
		Strategy:     VictimsLocal,
		MoveGate:     moveGate,
		NeedCPU:      needCPU,
		NeedMem:      needMem,
		Order:        order,
		MovedUIDs:    movedUIDs,
		Cap:          SolverLocalSearchMaxVictimsPerNode,
		Rng:          rng,
		RandomizePct: 25,
	})

	// NEW: respect -1/0/+N for per-target victim probes
	limit := SolverLocalSearchMaxVictimProbesPerTarget
	if limit == 0 {
		return nil, false // no probes at this target
	}

	attempts := 0
	for _, v := range victims {
		if limit > 0 && attempts >= limit {
			break
		}
		if _, seen := chosen[v.UID]; seen {
			continue
		}

		// (1) direct move v -> D (honor cap)
		if D := bestDest(v, target.Name, order, delta); D != nil {
			// FIX: only enforce when capMoves > 0
			if capMoves > 0 && len(moves)+1 > capMoves {
				return nil, false
			}
			moves = append(moves, MoveLite{UID: v.UID, From: target.Name, To: D.Name})
			addDelta(delta, target.Name, D.Name, v.ReqCPUm, v.ReqMemBytes)
			needCPU = max64(0, needCPU-v.ReqCPUm)
			needMem = max64(0, needMem-v.ReqMemBytes)
			chosen[v.UID] = struct{}{}
			if needCPU == 0 && needMem == 0 {
				return moves, true
			}
			attempts++
			continue
		}

		// (2) swap v <-> q (counts as 2 moves, honor cap)
		if B, q := bestSwap(target, v, needCPU, needMem, order, delta, moveGate, movedUIDs, chosen); B != nil && q != nil {
			// FIX: only enforce when capMoves > 0
			if capMoves > 0 && len(moves)+2 > capMoves {
				return nil, false
			}
			moves = append(moves,
				MoveLite{UID: v.UID, From: target.Name, To: B.Name},
				MoveLite{UID: q.UID, From: B.Name, To: target.Name},
			)
			da := delta[target.Name]
			db := delta[B.Name]
			da.CPU += v.ReqCPUm - q.ReqCPUm
			da.Mem += v.ReqMemBytes - q.ReqMemBytes
			db.CPU += q.ReqCPUm - v.ReqCPUm
			db.Mem += q.ReqMemBytes - v.ReqMemBytes
			delta[target.Name] = da
			delta[B.Name] = db

			// recompute remaining need
			needCPU = max64(0, pending.ReqCPUm-(target.AllocCPUm+delta[target.Name].CPU))
			needMem = max64(0, pending.ReqMemBytes-(target.AllocMemBytes+delta[target.Name].Mem))

			chosen[v.UID] = struct{}{}
			chosen[q.UID] = struct{}{}
			if needCPU == 0 && needMem == 0 {
				return moves, true
			}
			attempts++
			continue
		}

		attempts++ // counted even if neither move nor swap succeeded
	}

	return nil, false
}

// bestSwap finds the best swap of victim on target with some q on another node B,
// maximizing gain on target (limited by remaining need) while keeping both nodes ≥ 0.
// Among equals, prefer q that has already been moved in this batch.
func bestSwap(
	target *SolverNode,
	victim *SolverPod,
	needCPU, needMem int64,
	order []*SolverNode,
	delta map[string]Delta,
	moveGate *int32,
	movedUIDs map[string]struct{},
	chosen map[string]struct{},
) (*SolverNode, *SolverPod) {
	var bestB *SolverNode
	var bestQ *SolverPod
	bestGain := int64(math.MinInt64)

	for _, B := range order {
		if B.Name == target.Name {
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
			finalA_CPU := (target.AllocCPUm + delta[target.Name].CPU) + victim.ReqCPUm - q.ReqCPUm
			finalA_MEM := (target.AllocMemBytes + delta[target.Name].Mem) + victim.ReqMemBytes - q.ReqMemBytes
			finalB_CPU := (B.AllocCPUm + db.CPU) + q.ReqCPUm - victim.ReqCPUm
			finalB_MEM := (B.AllocMemBytes + db.Mem) + q.ReqMemBytes - victim.ReqMemBytes
			if finalA_CPU < 0 || finalA_MEM < 0 || finalB_CPU < 0 || finalB_MEM < 0 {
				continue
			}
			// Limit gains by the remaining need on A
			gainCPU := min64(victim.ReqCPUm, needCPU) - min64(q.ReqCPUm, needCPU)
			gainMEM := min64(victim.ReqMemBytes, needMem) - min64(q.ReqMemBytes, needMem)
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

// bestDest finds the best destination node for q (not src) where it fits under current delta,
// minimizing post-placement waste (CPU, then MEM, then name).
func bestDest(q *SolverPod, src string, order []*SolverNode, delta map[string]Delta) *SolverNode {
	var best *SolverNode
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.Name == src {
			continue
		}
		d := delta[n.Name]
		if (n.AllocCPUm+d.CPU) >= q.ReqCPUm && (n.AllocMemBytes+d.Mem) >= q.ReqMemBytes {
			cw := (n.AllocCPUm + d.CPU) - q.ReqCPUm
			mw := (n.AllocMemBytes + d.Mem) - q.ReqMemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && (best == nil || n.Name < best.Name)))) {
				best = n
				bestCPUWaste, bestMEMWaste = cw, mw
			}
		}
	}
	return best
}

// addDelta updates the delta map to reflect a move of cpu/mem from "from" to "to".
func addDelta(delta map[string]Delta, from, to string, cpu, mem int64) {
	addEdgeDelta(delta, from, to, cpu, mem)
}
