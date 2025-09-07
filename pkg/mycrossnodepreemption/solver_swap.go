package mycrossnodepreemption

import (
	"math"
	"math/rand"
	"sort"
	"time"
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
	return runSolverCommon(in,
		// Swap/Virtual planner
		func(p *SolverPod, nodes map[string]*SolverNode, pods map[string]*SolverPod, order []*SolverNode,
			moveGate *int32, movedUIDs map[string]struct{}, newPlacements map[string]string,
		) bool {
			return placeByMovesOnly(p, nodes, pods, order, moveGate, movedUIDs, newPlacements)
		},
		"swap",
	)
}

type Delta struct{ cpu, mem int64 }

func placeByMovesOnly(
	p *SolverPod,
	nodes map[string]*SolverNode,
	pods map[string]*SolverPod,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) bool {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	bestMoves, bestTarget, ok := bestPlanAcrossTargets(p, order, func(t *SolverNode) ([]moveLite, bool) {
		// Try a few fresh relocation attempts for this target node
		var best []moveLite
		bestCount := math.MaxInt32
		for trial := 0; trial < SolverSwapMaxTrialsPerPendingPod; trial++ {
			mvs, ok := planOnTargetVirtual(p, t, order, moveGate, movedUIDs, rng, SolverSwapMaxMovesForPendingPod)
			if ok && len(mvs) < bestCount {
				best, bestCount = mvs, len(mvs)
				if bestCount <= 1 {
					break
				}
			}
		}
		if best == nil {
			return nil, false
		}
		return best, true
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

func planOnTargetVirtual(
	p *SolverPod,
	A *SolverNode,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[string]struct{},
	rng *rand.Rand,
	capMoves int,
) ([]moveLite, bool) {
	needCPU := max64(0, p.ReqCPUm-A.AllocCPUm)
	needMem := max64(0, p.ReqMemBytes-A.AllocMemBytes)
	if needCPU == 0 && needMem == 0 {
		return nil, true
	}

	delta := map[string]Delta{}
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
			addDelta(delta, A.Name, D.Name, v.ReqCPUm, v.ReqMemBytes)
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
			da.cpu += v.ReqCPUm - q.ReqCPUm
			da.mem += v.ReqMemBytes - q.ReqMemBytes
			db.cpu += q.ReqCPUm - v.ReqCPUm
			db.mem += q.ReqMemBytes - v.ReqMemBytes
			delta[A.Name] = da
			delta[B.Name] = db

			// recompute remaining need for A under delta
			needCPU = max64(0, p.ReqCPUm-(A.AllocCPUm+delta[A.Name].cpu))
			needMem = max64(0, p.ReqMemBytes-(A.AllocMemBytes+delta[A.Name].mem))

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

func addDelta(delta map[string]Delta, from, to string, cpu, mem int64) {
	addEdgeDelta(delta, from, to, cpu, mem)
}

func bestDestUnderDelta(q *SolverPod, src string, order []*SolverNode, delta map[string]Delta) *SolverNode {
	var best *SolverNode
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.Name == src {
			continue
		}
		d := delta[n.Name]
		if (n.AllocCPUm+d.cpu) >= q.ReqCPUm && (n.AllocMemBytes+d.mem) >= q.ReqMemBytes {
			cw := (n.AllocCPUm + d.cpu) - q.ReqCPUm
			mw := (n.AllocMemBytes + d.mem) - q.ReqMemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && (best == nil || n.Name < best.Name)))) {
				best = n
				bestCPUWaste, bestMEMWaste = cw, mw
			}
		}
	}
	return best
}

func bestSwapUnderDelta(
	A *SolverNode,
	v *SolverPod,
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
			finalA_CPU := (A.AllocCPUm + delta[A.Name].cpu) + v.ReqCPUm - q.ReqCPUm
			finalA_MEM := (A.AllocMemBytes + delta[A.Name].mem) + v.ReqMemBytes - q.ReqMemBytes
			finalB_CPU := (B.AllocCPUm + db.cpu) + q.ReqCPUm - v.ReqCPUm
			finalB_MEM := (B.AllocMemBytes + db.mem) + q.ReqMemBytes - v.ReqMemBytes
			if finalA_CPU < 0 || finalA_MEM < 0 || finalB_CPU < 0 || finalB_MEM < 0 {
				continue
			}
			// Limit gains by the remaining need on A
			gainCPU := min64(v.ReqCPUm, needCPU) - min64(q.ReqCPUm, needCPU)
			gainMEM := min64(v.ReqMemBytes, needMem) - min64(q.ReqMemBytes, needMem)
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

func rankVictimsOnNode(
	A *SolverNode,
	needCPU, needMem int64,
	moveGate *int32,
	movedUIDs map[string]struct{},
	order []*SolverNode,
) []*SolverPod {
	cands := make([]*SolverPod, 0, len(A.Pods))
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
		q           *SolverPod
		singleCover bool
		relocCount  int
		overshoot   int64
		alreadyMv   bool
	}
	sc := make([]scored, 0, len(cands))
	for _, q := range cands {
		_, already := movedUIDs[q.UID]
		single := (q.ReqCPUm >= needCPU && q.ReqMemBytes >= needMem)
		ov := max64(0, q.ReqCPUm-needCPU) + max64(0, q.ReqMemBytes-needMem)
		rc := 0
		for _, n := range order {
			if n.Name == A.Name {
				continue
			}
			if n.canPodFit(q.ReqCPUm, q.ReqMemBytes) {
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
		si, sj := qi.ReqCPUm*qi.ReqMemBytes, qj.ReqCPUm*qj.ReqMemBytes
		if si != sj {
			return si > sj
		}
		if qi.ReqCPUm != qj.ReqCPUm {
			return qi.ReqCPUm < qj.ReqCPUm
		}
		if qi.ReqMemBytes != qj.ReqMemBytes {
			return qi.ReqMemBytes < qj.ReqMemBytes
		}
		return qi.UID < qj.UID
	})
	limit := SolverSwapMaxVictimsPerNode
	if limit > 0 && len(sc) > limit {
		sc = sc[:limit]
	}
	out := make([]*SolverPod, 0, len(sc))
	for _, s := range sc {
		out = append(out, s.q)
	}
	return out
}
