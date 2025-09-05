package mycrossnodepreemption

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

// ========================= Tunables & types =========================

// Simple random-swap strategy tunables
type swapCfg struct {
	// maximum number of pod moves allowed in a single random plan (a swap counts as 2)
	MaxMovesPerPlan int
	// maximum number of random swap plans to try before evicting one pod
	MaxSwapTrials int
}

var defaultSwapCfg = swapCfg{
	// Reasonable defaults for the random swapper
	MaxMovesPerPlan: 6,
	MaxSwapTrials:   1000, // number of random plans before evicting the lowest priority pod
}

// ============================= Entry ==============================

func runSolverSwap(in SolverInput) *SolverOutput {
	cfg := defaultSwapCfg

	// Build nodes
	nodes := make(map[string]*nLite, len(in.Nodes))
	order := make([]*nLite, 0, len(in.Nodes))
	for i := range in.Nodes {
		n := &nLite{
			Name:    in.Nodes[i].Name,
			CapCPU:  in.Nodes[i].CPUm,
			CapMem:  in.Nodes[i].MemBytes,
			FreeCPU: in.Nodes[i].CPUm,
			FreeMem: in.Nodes[i].MemBytes,
			Pods:    make(map[string]*pLite, 32),
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })

	// Build pods
	all := make(map[string]*pLite, len(in.Pods)+1)
	var pre *pLite
	if in.Preemptor != nil {
		pre = &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
			Node:      "",
			origNode:  "",
		}
		all[pre.UID] = pre
	}
	for i := range in.Pods {
		sp := in.Pods[i]
		p := &pLite{
			UID:       sp.UID,
			CPUm:      sp.CPU_m,
			MemBytes:  sp.MemBytes,
			Priority:  sp.Priority,
			Protected: sp.Protected,
			Node:      sp.Where,
			origNode:  sp.Where,
		}
		all[p.UID] = p
		if p.Node != "" {
			if n := nodes[p.Node]; n != nil {
				n.add(p)
			}
		}
	}

	placements := make(map[string]string)
	var evicts []Placement

	if pre == nil {
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	// 1) Direct fit
	if to, ok := bestDirectFit(order, pre); ok {
		klog.InfoS("direct-fit", "preemptor", pre.UID, "to", to)
		nodes[to].add(pre)
		placements[pre.UID] = to
		return stableOutput("FEASIBLE", placements, evicts, in)
	}

	// 2) Simple random-swap search with fallback to global lowest-priority eviction
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	triedPlans := map[string]struct{}{} // dedupe exact same swap outcomes across trials

	for {
		// FIX: pass rng before triedPlans (matches tryRandomSwapPlans signature)
		bestMoves, bestTarget, found := tryRandomSwapPlans(nodes, all, order, pre, cfg, rng, triedPlans)
		if found {
			// Apply best movement plan (coalesced) and place preemptor
			if !verifyCoalescedPlan(nodes, all, bestMoves, nil, "") {
				klog.InfoS("random-swap: best plan failed verification, falling back to eviction",
					"moves", len(bestMoves))
			} else if applyTwoPhase(nodes, all, bestMoves) {
				for _, mv := range bestMoves {
					placements[mv.UID] = mv.To
				}
				if n := nodes[bestTarget]; n != nil && n.fits(pre.CPUm, pre.MemBytes) {
					n.add(pre)
					placements[pre.UID] = n.Name
					return stableOutput("FEASIBLE", placements, evicts, in)
				}
				// As a safety, re-check best direct fit after the moves applied
				if to, ok := bestDirectFit(order, pre); ok {
					nodes[to].add(pre)
					placements[pre.UID] = to
					return stableOutput("FEASIBLE", placements, evicts, in)
				}
				klog.InfoS("random-swap: inconsistent placement after plan; attempting eviction")
			}
		}

		// No plan found or plan could not be applied -> evict lowest globally priority pod and try again
		v, on := pickLowestPriorityGlobal(order, pre)
		if v == nil || on == nil {
			return stableOutput("INFEASIBLE", placements, evicts, in)
		}
		on.remove(v)
		evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
		klog.InfoS("evict-lowest-priority-and-retry", "victim", v.UID, "prio", v.Priority, "from", on.Name)

		// After eviction, try again: (quick direct fit first)
		if to, ok := bestDirectFit(order, pre); ok {
			nodes[to].add(pre)
			placements[pre.UID] = to
			return stableOutput("FEASIBLE", placements, evicts, in)
		}
		// Clear tried plans after eviction because the state space has changed
		triedPlans = map[string]struct{}{}
	}
}

// ========================= Simple Random Swap Core =========================

// tryRandomSwapPlans attempts up to cfg.MaxSwapTrials random plans consisting of up to cfg.MaxMovesPerPlan pod moves.
// A swap counts as 2 moves. At each step it checks whether any node would fit the preemptor after applying the current plan.
// It remembers the best (fewest moves) successful plan and returns it coalesced.
// triedPlans is a cross-trial dedup set keyed by canonical plan signature (final from->to pairs).
func tryRandomSwapPlans(
	nodes map[string]*nLite,
	all map[string]*pLite,
	order []*nLite,
	pre *pLite,
	cfg swapCfg,
	rng *rand.Rand,
	triedPlans map[string]struct{},
) ([]moveLite, string, bool) {
	type d struct{ cpu, mem int64 }

	bestMoves := []moveLite(nil)
	bestTarget := ""
	bestCount := math.MaxInt32
	found := false

	// Helper: build "orig" snapshot from current state
	buildOrig := func() map[string]string {
		orig := map[string]string{}
		for _, p := range all {
			if p.Node != "" {
				orig[p.UID] = p.Node
			}
		}
		return orig
	}

	// Helper: build finalDest from final locations vs orig
	finalDestFromFinalLoc := func(finalLoc map[string]string, orig map[string]string) map[string]string {
		fd := map[string]string{}
		for uid, from := range orig {
			to := finalLoc[uid]
			if to != "" && to != from {
				fd[uid] = to
			}
		}
		return fd
	}

	// Helper: signature for a set of coalesced moves
	signatureFromMoves := func(moves []moveLite) string {
		cp := make([]moveLite, len(moves))
		copy(cp, moves)
		sort.Slice(cp, func(i, j int) bool {
			if cp[i].UID != cp[j].UID {
				return cp[i].UID < cp[j].UID
			}
			if cp[i].From != cp[j].From {
				return cp[i].From < cp[j].From
			}
			return cp[i].To < cp[j].To
		})
		s := ""
		for _, mv := range cp {
			s += mv.UID + ":" + mv.From + "->" + mv.To + ";"
		}
		return s
	}

	// Helper: check if after current deltas any node fits preemptor
	checkFitAfterDeltas := func(delta map[string]d) (string, bool) {
		for _, n := range order {
			df := delta[n.Name]
			fCPU := n.FreeCPU + df.cpu
			fMem := n.FreeMem + df.mem
			if fCPU >= pre.CPUm && fMem >= pre.MemBytes {
				return n.Name, true
			}
		}
		return "", false
	}

	// Build list of movable pods (eligible)
	isMovable := func(p *pLite) bool {
		if p == nil || p.Node == "" || p.Protected {
			return false
		}
		// only allow moving pods with priority <= preemptor's priority
		if p.Priority > pre.Priority {
			return false
		}
		return true
	}

	// Trial loop
	for trial := 0; trial < cfg.MaxSwapTrials; trial++ {
		orig := buildOrig()
		// Virtual state trackers
		delta := map[string]d{}            // per-node net free delta after planned moves
		finalLoc := map[string]string{}    // planned final location per pod UID
		chosenUID := map[string]struct{}{} // avoid picking same pod multiple times in one plan
		for uid, from := range orig {
			finalLoc[uid] = from
		}

		planMoves := make([]moveLite, 0, cfg.MaxMovesPerPlan)
		steps := 0

		tryStep := func() bool {
			// attempt a direct move or a swap; limit attempts to avoid infinite loops on tight configs
			for attempt := 0; attempt < 16; attempt++ {
				// Randomly choose action: 0=direct, 1=swap
				action := rng.Intn(2)

				// Collect eligible pods
				eligible := make([]*pLite, 0, 64)
				for _, n := range order {
					for _, p := range n.Pods {
						if !isMovable(p) {
							continue
						}
						if _, seen := chosenUID[p.UID]; seen {
							continue
						}
						eligible = append(eligible, p)
					}
				}
				if len(eligible) == 0 {
					return false
				}
				p := eligible[rng.Intn(len(eligible))]
				fromA := finalLoc[p.UID]
				// pick a different destination node at random
				destIdx := rng.Intn(maxInt(1, len(order)))
				for tries := 0; tries < len(order); tries++ {
					dn := order[(destIdx+tries)%len(order)]
					if dn.Name == fromA {
						continue
					}
					// current deltas
					da := delta[fromA]
					db := delta[dn.Name]

					if action == 0 {
						// direct move p: A -> B if final non-negative on B
						if (dn.FreeCPU+db.cpu) >= p.CPUm && (dn.FreeMem+db.mem) >= p.MemBytes {
							// apply
							planMoves = append(planMoves, moveLite{UID: p.UID, From: fromA, To: dn.Name})
							delta[fromA] = d{cpu: da.cpu + p.CPUm, mem: da.mem + p.MemBytes}
							delta[dn.Name] = d{cpu: db.cpu - p.CPUm, mem: db.mem - p.MemBytes}
							finalLoc[p.UID] = dn.Name
							chosenUID[p.UID] = struct{}{}
							return true
						}
					} else {
						// swap: pick q on B and check final feasibility (no intermediate fit required)
						// gather eligible q on B
						candsQ := make([]*pLite, 0, len(dn.Pods))
						for _, q := range dn.Pods {
							if !isMovable(q) {
								continue
							}
							if _, seen := chosenUID[q.UID]; seen {
								continue
							}
							candsQ = append(candsQ, q)
						}
						if len(candsQ) == 0 {
							continue
						}
						q := candsQ[rng.Intn(len(candsQ))]
						fromB := finalLoc[q.UID]

						// final deltas after swap p:A->B and q:B->A
						// A: +p - q; B: +q - p
						finalA_CPU := (nodes[fromA].FreeCPU + da.cpu) + p.CPUm - q.CPUm
						finalA_MEM := (nodes[fromA].FreeMem + da.mem) + p.MemBytes - q.MemBytes
						finalB_CPU := (dn.FreeCPU + db.cpu) + q.CPUm - p.CPUm
						finalB_MEM := (dn.FreeMem + db.mem) + q.MemBytes - p.MemBytes

						if finalA_CPU >= 0 && finalA_MEM >= 0 && finalB_CPU >= 0 && finalB_MEM >= 0 {
							// apply both moves
							planMoves = append(planMoves,
								moveLite{UID: p.UID, From: fromA, To: dn.Name},
								moveLite{UID: q.UID, From: fromB, To: fromA},
							)
							delta[fromA] = d{cpu: da.cpu + p.CPUm - q.CPUm, mem: da.mem + p.MemBytes - q.MemBytes}
							delta[dn.Name] = d{cpu: db.cpu + q.CPUm - p.CPUm, mem: db.mem + q.MemBytes - p.MemBytes}
							finalLoc[p.UID] = dn.Name
							finalLoc[q.UID] = fromA
							chosenUID[p.UID] = struct{}{}
							chosenUID[q.UID] = struct{}{}
							return true
						}
					}
				}
			}
			return false
		}

		for steps < cfg.MaxMovesPerPlan {
			if !tryStep() {
				break
			}
			steps = len(planMoves) // each direct adds 1, swap adds 2

			// After each addition (move or swap), see if preemptor would fit on any node
			if tgt, ok := checkFitAfterDeltas(delta); ok {
				// Build coalesced plan from current finalLoc vs orig
				fd := finalDestFromFinalLoc(finalLoc, orig)
				coalesced := squashMoves(fd, orig)

				// Dedup: if we already tried exactly this plan, skip saving
				sig := signatureFromMoves(coalesced)
				if _, seen := triedPlans[sig]; !seen {
					triedPlans[sig] = struct{}{}
					if len(coalesced) < bestCount {
						bestCount = len(coalesced)
						bestMoves = coalesced
						bestTarget = tgt
						found = true
						klog.V(2).InfoS("random-swap: candidate plan", "moves", bestCount, "target", bestTarget)
						// early exit if zero (should not happen) or 1 is the theoretical best we can do within this loop
						if bestCount <= 1 {
							return bestMoves, bestTarget, found
						}
					}
				}
			}

			// If we've already found a plan with X moves, adding more moves in this trial cannot beat it.
			if found && len(planMoves) >= bestCount {
				break
			}
		}

		// At the end of a trial, also mark the plan we attempted (even if it didn't yield a fit) to avoid repeating exact swaps
		fd := map[string]string{}
		for uid, from := range orig {
			if to := finalLoc[uid]; to != "" && to != from {
				fd[uid] = to
			}
		}
		coalesced := squashMoves(fd, orig)
		sig := signatureFromMoves(coalesced)
		triedPlans[sig] = struct{}{}
	}

	return bestMoves, bestTarget, found
}

// ========================= Helpers / scoring =========================

// New: pick globally lowest-priority eligible pod to evict (priority strictly lower than preemptor, and not protected)
func pickLowestPriorityGlobal(order []*nLite, pre *pLite) (*pLite, *nLite) {
	var bestPod *pLite
	var bestNode *nLite
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Protected {
				continue
			}
			// In preemption semantics, only lower-priority pods are eligible
			if q.Priority >= pre.Priority {
				continue
			}
			if bestPod == nil || q.Priority < bestPod.Priority ||
				(q.Priority == bestPod.Priority && (q.CPUm+q.MemBytes) > (bestPod.CPUm+bestPod.MemBytes)) {
				bestPod = q
				bestNode = n
			}
		}
	}
	return bestPod, bestNode
}
