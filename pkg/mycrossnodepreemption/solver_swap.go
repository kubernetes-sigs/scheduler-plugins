package mycrossnodepreemption

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

// When we can't place a pod and need an eviction,
// we should actually remove one of the previous moved, as they then will not count as a move.
// We can do: (1) direct move of a pod to another (best option, only one move), or (2) swap pods between nodes (if it frees resources on one of the nodes).
// When we fallback to eviction, we should first try to evict one of the already moved pods (as they don't count as a move anymore).

func runSolverSwap(in SolverInput) *SolverOutput {
	nodes, allPods, pending, order, _ := buildClusterState(in)

	newPlacements := make(map[string]string)
	var evicts []Placement

	if len(pending) == 0 {
		klog.InfoS("random-swap: nothing to place")
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	klog.InfoS("random-swap: start")

	// Worklist + gating policy (nil gate in batch mode == no priority restriction).
	var (
		worklist []*pLite
		single   bool
		gatePtr  *int32
	)
	if in.Preemptor != nil {
		for _, p := range pending {
			if p.UID == in.Preemptor.UID {
				worklist = []*pLite{p}
				single = true
				g := in.Preemptor.Priority
				gatePtr = &g // restrict moves/evictions to <= / <
				break
			}
		}
	}
	if !single {
		worklist = append(worklist, pending...)
		sortPodsByPriorityDescThenSmallestFirst(worklist)
		gatePtr = nil // batch: unrestricted (still respects Protected)
	}

	// Prefer already-moved pods within a batch; avoid evicting them.
	movedUIDs := make(map[string]struct{})

	// Helper: after a plan is applied, try preferred target, else best fit.
	placeAfterMoves := func(p *pLite, prefer string) bool {
		if prefer != "" {
			if n := nodes[prefer]; n != nil && n.fits(p.CPUm, p.MemBytes) {
				n.addPod(p)
				newPlacements[p.UID] = n.Name
				return true
			}
		}
		if to, ok := bestDirectFit(order, p); ok {
			nodes[to].addPod(p)
			newPlacements[p.UID] = to
			return true
		}
		return false
	}

	// Place a single pod using labels (tryDirect → tryMoves → tryEvict → tryDirect…)
	placeOne := func(p *pLite) bool {
		evicted := 0

	tryDirect:
		if to, ok := bestDirectFit(order, p); ok {
			nodes[to].addPod(p)
			newPlacements[p.UID] = to
			return true
		}
		goto tryMoves

	tryMoves:
		tried := map[string]struct{}{}
		mvs, assign, found, _ := tryRandomSwapPlansBatch(
			nodes, allPods, order, []*pLite{p}, rng, tried, gatePtr, movedUIDs,
		)
		if found && verifyCoalescedPlan(nodes, allPods, mvs, nil, "") && applyTwoPhase(nodes, allPods, mvs) {
			for _, mv := range mvs {
				newPlacements[mv.UID] = mv.To
				movedUIDs[mv.UID] = struct{}{}
			}
			if placeAfterMoves(p, assign[p.UID]) {
				return true
			}
			// fallthrough → eviction
		}
		goto tryEvict

	tryEvict:
		if evicted >= SolverSwapMaxEvictionsPerPod {
			return false
		}

		// (A) First prefer evicting a pod we already moved in this batch
		//     *if* that single eviction enables a direct fit for `p`.
		if mv, on := pickEvictionPreferMovedEnablingDirectFit(order, gatePtr, movedUIDs, p); mv != nil && on != nil {
			on.remove(mv)
			// Undo "move counting" for that pod: drop its placement entry and
			// from the moved set so it doesn't look like a move+evict.
			delete(newPlacements, mv.UID)
			delete(movedUIDs, mv.UID)
			evicts = append(evicts, Placement{Pod: Pod{UID: mv.UID}, Node: on.Name})
			evicted++
			goto tryDirect
		}

		// (B) Otherwise, behave as before: pick global lowest-priority victim,
		//     still avoiding already-moved pods.
		if v, on := pickLowestPriorityGlobalForBatch(order, gatePtr, movedUIDs); v != nil && on != nil {
			on.remove(v)
			evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
			evicted++
			goto tryDirect
		}

		return false
	}

	// Execute
	if single {
		if !placeOne(worklist[0]) {
			klog.InfoS("random-swap: single preemptor infeasible under <=-move/<-evict gate",
				"preemptor", worklist[0].UID, "prio", in.Preemptor.Priority)
			return stableOutput("INFEASIBLE", newPlacements, evicts, in)
		}
		return stableOutput("FEASIBLE", newPlacements, evicts, in)
	}

	// Batch: process prio-desc; stop at first failure and discard ≤ that prio.
	var stop bool
	var stopAt int32
	for _, p := range worklist {
		if stop && p.Priority <= stopAt {
			break
		}
		if ok := placeOne(p); !ok {
			stop = true
			stopAt = p.Priority
			klog.InfoS("random-swap: stopping batch at priority; discarding remainder",
				"priority", stopAt, "uid", p.UID)
			break
		}
	}

	return stableOutput("FEASIBLE", newPlacements, evicts, in)
}

// Batch fit on current residuals (Free + delta), returns assignment for ALL pendingPods if possible.
func batchFitOnResidual(order []*nLite, delta map[string]deltaDM, pendingPods []*pLite) (map[string]string, bool) {
	type capDM struct{ cpu, mem int64 }

	// Compute residual capacity per node
	resid := make(map[string]capDM, len(order))
	for _, n := range order {
		d := delta[n.Name]
		resid[n.Name] = capDM{cpu: n.FreeCPUm + d.cpu, mem: n.FreeMemBytes + d.mem}
	}

	// Best-Fit-Decreasing on these residuals
	cand := make([]*pLite, len(pendingPods))
	copy(cand, pendingPods)
	sort.Slice(cand, func(i, j int) bool {
		ai, aj := cand[i], cand[j]
		si := ai.CPUm * ai.MemBytes
		sj := aj.CPUm * aj.MemBytes
		if si != sj {
			return si > sj
		}
		if ai.CPUm != aj.CPUm {
			return ai.CPUm > aj.CPUm
		}
		if ai.MemBytes != aj.MemBytes {
			return ai.MemBytes > aj.MemBytes
		}
		return ai.UID < aj.UID
	})

	assign := make(map[string]string, len(cand))
	for _, p := range cand {
		bestNode := ""
		bestCPUWaste := int64(math.MaxInt64)
		bestMEMWaste := int64(math.MaxInt64)
		for _, n := range order {
			r := resid[n.Name]
			if r.cpu >= p.CPUm && r.mem >= p.MemBytes {
				wc := r.cpu - p.CPUm
				wm := r.mem - p.MemBytes
				if wc < bestCPUWaste || (wc == bestCPUWaste && (wm < bestMEMWaste || (wm == bestMEMWaste && n.Name < bestNode))) {
					bestNode, bestCPUWaste, bestMEMWaste = n.Name, wc, wm
				}
			}
		}
		if bestNode == "" {
			// LOG FAILURE REASON
			klog.V(2).InfoS("batch-fit: cannot-place", "uid", p.UID, "needCPU", p.CPUm, "needMem", p.MemBytes)
			for _, n := range order {
				r := resid[n.Name]
				klog.V(2).InfoS("batch-fit: node-free", "node", n.Name, "cpu", r.cpu, "mem", r.mem)
			}
			return nil, false
		}
		assign[p.UID] = bestNode
		r := resid[bestNode]
		resid[bestNode] = capDM{cpu: r.cpu - p.CPUm, mem: r.mem - p.MemBytes}
	}
	return assign, true
}

//
// ========================= Misc helpers (priority, eviction, etc.) =========================
//

// Sort by (Priority desc, size ASC, UID asc)
func sortPodsByPriorityDescThenSmallestFirst(pods []*pLite) {
	sort.Slice(pods, func(i, j int) bool {
		a, b := pods[i], pods[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority // higher prio first
		}
		// size = CPU * MEM; smaller first
		sa := a.CPUm * a.MemBytes
		sb := b.CPUm * b.MemBytes
		if sa != sb {
			return sa < sb
		}
		// tie-breakers: smaller CPU, then smaller MEM, then UID
		if a.CPUm != b.CPUm {
			return a.CPUm < b.CPUm
		}
		if a.MemBytes != b.MemBytes {
			return a.MemBytes < b.MemBytes
		}
		return a.UID < b.UID
	})
}

// Movement gate: allow moving pods with priority <= gate
func canMoveWithGate(p *pLite, gate *int32) bool {
	if p == nil || p.Node == "" || p.Protected {
		return false
	}
	if gate == nil {
		return true
	} // defensive
	return p.Priority <= *gate
}

// Eviction gate: only strictly lower than gate
func canEvictWithGate(p *pLite, gate *int32) bool {
	if p == nil || p.Protected {
		return false
	}
	if gate == nil {
		return true
	} // defensive
	return p.Priority < *gate
}

// Evict globally lowest-priority eligible pod for the batch (priority strictly lower than max pendingPods).
func pickLowestPriorityGlobalForBatch(
	order []*nLite,
	allowPriPtr *int32,
	avoid map[string]struct{}, // don't evict already-moved UIDs
) (*pLite, *nLite) {
	var bestPod *pLite
	var bestNode *nLite
	for _, n := range order {
		for _, q := range n.Pods {
			if !canEvictWithGate(q, allowPriPtr) {
				continue
			}
			if _, moved := avoid[q.UID]; moved {
				continue
			} // don't evict already-moved
			if bestPod == nil || q.Priority < bestPod.Priority ||
				(q.Priority == bestPod.Priority && (q.CPUm+q.MemBytes) > (bestPod.CPUm+bestPod.MemBytes)) {
				bestPod, bestNode = q, n
			}
		}
	}
	return bestPod, bestNode
}

type plannerStats struct {
	trials       int
	improvements int
	dedupSkips   int
	lastEligible int
}

// ---------- compact random-swap planner (drop-in) ----------
func tryRandomSwapPlansBatch(
	nodes map[string]*nLite,
	all map[string]*pLite,
	order []*nLite,
	incoming []*pLite,
	rng *rand.Rand,
	triedPlans map[string]struct{},
	allowPriPtr *int32,
	preferUIDs map[string]struct{},
) ([]moveLite, map[string]string, bool, plannerStats) {

	type sel = struct {
		p     *pLite
		from  string
		fromN *nLite
	}
	stats := plannerStats{}

	// ------------ tiny helpers ------------
	buildOrig := func() map[string]string {
		orig := map[string]string{}
		for _, p := range all {
			if p.Node != "" {
				orig[p.UID] = p.Node
			}
		}
		return orig
	}
	ensureAllInFinal := func(finalLoc map[string]string) {
		for _, n := range order {
			for _, q := range n.Pods {
				if q.Node != "" {
					if _, ok := finalLoc[q.UID]; !ok {
						finalLoc[q.UID] = q.Node
					}
				}
			}
		}
	}
	prefLess := func(a, b *pLite) bool {
		_, pa := preferUIDs[a.UID]
		_, pb := preferUIDs[b.UID]
		if pa != pb {
			return pa
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		sa, sb := a.CPUm*a.MemBytes, b.CPUm*b.MemBytes
		if sa != sb {
			return sa > sb
		}
		return a.UID < b.UID
	}
	sortByPref := func(list []*pLite) {
		sort.SliceStable(list, func(i, j int) bool { return prefLess(list[i], list[j]) })
	}
	isMovable := func(p *pLite) bool { return canMoveWithGate(p, allowPriPtr) }

	addMoveDelta := func(delta map[string]deltaDM, from, to string, cpu, mem int64) {
		da := delta[from]
		da.cpu += cpu
		da.mem += mem
		delta[from] = da
		db := delta[to]
		db.cpu -= cpu
		db.mem -= mem
		delta[to] = db
	}
	residCPU := func(n *nLite, d deltaDM) int64 { return n.FreeCPUm + d.cpu }
	residMem := func(n *nLite, d deltaDM) int64 { return n.FreeMemBytes + d.mem }

	finalDestFromFinalLoc := func(finalLoc, orig map[string]string) map[string]string {
		fd := map[string]string{}
		for uid, from := range orig {
			if to := finalLoc[uid]; to != "" && to != from {
				fd[uid] = to
			}
		}
		return fd
	}
	signatureFromMovesPlusAssign := func(moves []moveLite, assign map[string]string) string {
		cp := append([]moveLite(nil), moves...)
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
		ids := make([]string, 0, len(assign))
		for uid := range assign {
			ids = append(ids, uid)
		}
		sort.Strings(ids)
		s += "#"
		for _, uid := range ids {
			s += uid + "@" + assign[uid] + ";"
		}
		return s
	}
	validateEndpoints := func(mvs []moveLite) bool {
		for _, mv := range mvs {
			if nodes[mv.From] == nil || nodes[mv.To] == nil {
				return false
			}
		}
		return true
	}
	checkBatchFitAfterDeltas := func(delta map[string]deltaDM) (map[string]string, bool) {
		return batchFitOnResidual(order, delta, incoming)
	}

	// Reusable enumeration of movable pods (not already chosen, with known from)
	buildEligible := func(finalLoc map[string]string, chosen map[string]struct{}) []sel {
		out := make([]sel, 0, 64)
		for _, n := range order {
			for _, q := range n.Pods {
				if !isMovable(q) {
					continue
				}
				if q.Node == "" {
					continue
				}
				if _, seen := chosen[q.UID]; seen {
					continue
				}
				fromA := finalLoc[q.UID]
				if fromA == "" {
					continue
				}
				out = append(out, sel{p: q, from: fromA, fromN: nodes[fromA]})
			}
		}
		// keep only pod dimension for sorting; then rebuild sel order in-place
		plist := make([]*pLite, 0, len(out))
		for _, s := range out {
			plist = append(plist, s.p)
		}
		sortByPref(plist)
		idx := map[string]sel{}
		for _, s := range out {
			idx[s.p.UID] = s
		}
		out = out[:0]
		for _, p := range plist {
			out = append(out, idx[p.UID])
		}
		return out
	}

	// ---------- search ----------
	bestMoves := []moveLite(nil)
	bestAssign := map[string]string(nil)
	bestCount := math.MaxInt32
	found := false

	for trial := 0; trial < SolverSwapMaxSwapTrials; trial++ {
		stats.trials++

		orig := buildOrig()
		delta := map[string]deltaDM{}
		finalLoc := map[string]string{}
		for uid, from := range orig {
			finalLoc[uid] = from
		}
		ensureAllInFinal(finalLoc)

		chosen := map[string]struct{}{}
		planMoves := make([]moveLite, 0, SolverSwapMaxMovesPerPlan)

		tryStep := func() bool {
			el := buildEligible(finalLoc, chosen)
			stats.lastEligible = len(el)
			if len(el) == 0 {
				klog.V(2).InfoS("random-swap: no-eligible-movable-pods")
				return false
			}

			for attempt := 0; attempt < 16; attempt++ {
				s := el[rng.Intn(len(el))]
				p, fromA, na := s.p, s.from, s.fromN
				if na == nil {
					continue
				}

				// pick a different destination node (round-robin from random start)
				start := rng.Intn(maxInt(1, len(order)))
				for t := 0; t < len(order); t++ {
					dst := order[(start+t)%len(order)]
					if dst.Name == fromA {
						continue
					}
					da, db := delta[fromA], delta[dst.Name]

					// Action 1: DIRECT
					if rng.Intn(2) == 0 {
						if residCPU(dst, db) >= p.CPUm && residMem(dst, db) >= p.MemBytes {
							planMoves = append(planMoves, moveLite{UID: p.UID, From: fromA, To: dst.Name})
							addMoveDelta(delta, fromA, dst.Name, p.CPUm, p.MemBytes)
							finalLoc[p.UID] = dst.Name
							chosen[p.UID] = struct{}{}
							return true
						}
					} else {
						// Action 2: SWAP with q on dst
						qcands := make([]*pLite, 0, len(dst.Pods))
						for _, q := range dst.Pods {
							if !isMovable(q) {
								continue
							}
							if _, seen := chosen[q.UID]; seen {
								continue
							}
							if finalLoc[q.UID] == "" {
								continue
							}
							qcands = append(qcands, q)
						}
						if len(qcands) == 0 {
							continue
						}
						sortByPref(qcands)
						q := qcands[rng.Intn(len(qcands))]
						fromB := finalLoc[q.UID]
						nb := nodes[fromB]
						if nb == nil {
							continue
						}

						// residuals after swap
						finalA_CPU := residCPU(na, da) + p.CPUm - q.CPUm
						finalA_MEM := residMem(na, da) + p.MemBytes - q.MemBytes
						finalB_CPU := residCPU(dst, db) + q.CPUm - p.CPUm
						finalB_MEM := residMem(dst, db) + q.MemBytes - p.MemBytes

						if finalA_CPU >= 0 && finalA_MEM >= 0 && finalB_CPU >= 0 && finalB_MEM >= 0 {
							planMoves = append(planMoves,
								moveLite{UID: p.UID, From: fromA, To: dst.Name},
								moveLite{UID: q.UID, From: fromB, To: fromA},
							)
							// delta[fromA] += p - q; delta[dst] += q - p
							da.cpu += p.CPUm - q.CPUm
							da.mem += p.MemBytes - q.MemBytes
							db.cpu += q.CPUm - p.CPUm
							db.mem += q.MemBytes - p.MemBytes
							delta[fromA] = da
							delta[dst.Name] = db

							finalLoc[p.UID] = dst.Name
							finalLoc[q.UID] = fromA
							chosen[p.UID] = struct{}{}
							chosen[q.UID] = struct{}{}
							return true
						}
					}
				}
				// try another (p, action) pair
			}
			return false
		}

		for len(planMoves) < SolverSwapMaxMovesPerPlan {
			if !tryStep() {
				break
			}

			// After each step, see if ALL incoming fit on residuals.
			if assign, ok := checkBatchFitAfterDeltas(delta); ok {
				fd := finalDestFromFinalLoc(finalLoc, orig)
				coalesced := squashMoves(fd, orig)
				if !validateEndpoints(coalesced) {
					continue
				}
				sig := signatureFromMovesPlusAssign(coalesced, assign)
				if _, seen := triedPlans[sig]; seen {
					stats.dedupSkips++
				} else {
					triedPlans[sig] = struct{}{}
					if len(coalesced) < bestCount {
						bestCount = len(coalesced)
						bestMoves = coalesced
						bestAssign = assign
						found = true
						stats.improvements++
						if bestCount <= 1 {
							return bestMoves, bestAssign, found, stats
						}
					}
				}
			}

			// If we already have a plan with X moves, don't extend beyond X.
			if found && len(planMoves) >= bestCount {
				break
			}
		}

		// Mark this (unsuccessful) finalLoc as tried so we don't repeat it next trial.
		fd := map[string]string{}
		for uid, from := range orig {
			if to := finalLoc[uid]; to != "" && to != from {
				fd[uid] = to
			}
		}
		coalesced := squashMoves(fd, orig)
		triedPlans[signatureFromMovesPlusAssign(coalesced, map[string]string{})] = struct{}{}
	}

	return bestMoves, bestAssign, found, stats
}

type deltaDM struct{ cpu, mem int64 }

func pickEvictionPreferMovedEnablingDirectFit(
	order []*nLite,
	allowPriPtr *int32,
	moved map[string]struct{},
	need *pLite,
) (*pLite, *nLite) {

	var bestPod *pLite
	var bestNode *nLite

	for _, n := range order {
		for _, q := range n.Pods {
			if _, wasMoved := moved[q.UID]; !wasMoved {
				continue
			}
			if !canEvictWithGate(q, allowPriPtr) {
				continue
			}
			// Would evicting q on n make the needed pod fit here?
			if n.FreeCPUm+q.CPUm >= need.CPUm && n.FreeMemBytes+q.MemBytes >= need.MemBytes {
				// Keep the same tiebreakers you already use.
				if bestPod == nil || q.Priority < bestPod.Priority ||
					(q.Priority == bestPod.Priority && (q.CPUm+q.MemBytes) > (bestPod.CPUm+bestPod.MemBytes)) {
					bestPod, bestNode = q, n
				}
			}
		}
	}
	return bestPod, bestNode
}
