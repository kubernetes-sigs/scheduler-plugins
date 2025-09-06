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
//

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
		v, on := pickLowestPriorityGlobalForBatch(order, gatePtr, movedUIDs)
		if v == nil || on == nil {
			return false
		}
		on.remove(v)
		evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
		evicted++
		goto tryDirect
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

func tryRandomSwapPlansBatch(
	nodes map[string]*nLite,
	all map[string]*pLite,
	order []*nLite,
	pendingPods []*pLite,
	rng *rand.Rand,
	triedPlans map[string]struct{},
	allowPriPtr *int32,
	preferUIDs map[string]struct{}, // prefer already-moved pods
) ([]moveLite, map[string]string, bool, plannerStats) {

	bestMoves := []moveLite(nil)
	bestAssign := map[string]string(nil)
	bestCount := math.MaxInt32
	found := false

	stats := plannerStats{}

	// Helpers
	buildOrig := func() map[string]string {
		orig := map[string]string{}
		for _, p := range all {
			if p.Node != "" {
				orig[p.UID] = p.Node
			}
		}
		return orig
	}
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
	checkBatchFitAfterDeltas := func(delta map[string]deltaDM) (map[string]string, bool) {
		return batchFitOnResidual(order, delta, pendingPods)
	}

	// Movability gate: if allowPriPtr is nil => no gating; else use preemptor priority
	isMovable := func(p *pLite) bool {
		return canMoveWithGate(p, allowPriPtr)
	}

	for trial := 0; trial < SolverSwapMaxSwapTrials; trial++ {
		stats.trials++
		orig := buildOrig()

		// Virtual state trackers for this trial
		delta := map[string]deltaDM{}      // per-node free deltas
		finalLoc := map[string]string{}    // virtual final location per UID
		chosenUID := map[string]struct{}{} // avoid picking same UID multiple times in plan
		for uid, from := range orig {
			finalLoc[uid] = from
		}
		// Ensure finalLoc has entries for every pod currently on nodes
		for _, n := range order {
			for _, q := range n.Pods {
				if q.Node == "" {
					continue
				}
				if _, ok := finalLoc[q.UID]; !ok {
					finalLoc[q.UID] = q.Node
				}
			}
		}

		planMoves := make([]moveLite, 0, SolverSwapMaxMovesPerPlan)

		tryStep := func() bool {
			// Build eligible list (movable, not already chosen, with a valid location)
			eligible := make([]*pLite, 0, 64)
			for _, n := range order {
				for _, q := range n.Pods {
					if !isMovable(q) {
						continue
					}
					if _, seen := chosenUID[q.UID]; seen {
						continue
					}
					if q.Node == "" {
						continue
					}
					if loc := finalLoc[q.UID]; loc == "" {
						continue
					}
					eligible = append(eligible, q)
				}
			}
			// prefer already-moved first; then lower priority; then bigger pods
			sort.SliceStable(eligible, func(i, j int) bool {
				_, pi := preferUIDs[eligible[i].UID]
				_, pj := preferUIDs[eligible[j].UID]
				if pi != pj {
					return pi
				}
				if eligible[i].Priority != eligible[j].Priority {
					return eligible[i].Priority < eligible[j].Priority
				}
				si := eligible[i].CPUm * eligible[i].MemBytes
				sj := eligible[j].CPUm * eligible[j].MemBytes
				if si != sj {
					return si > sj
				}
				return eligible[i].UID < eligible[j].UID
			})
			stats.lastEligible = len(eligible)
			if len(eligible) == 0 {
				klog.V(2).InfoS("random-swap: no-eligible-movable-pods", "trial", trial)
				return false
			}

			for attempt := 0; attempt < 16; attempt++ {
				action := rng.Intn(2) // 0=direct, 1=swap
				p := eligible[rng.Intn(len(eligible))]
				fromA := finalLoc[p.UID]
				if fromA == "" {
					klog.V(2).InfoS("random-swap: skip-eligible-without-valid-fromA", "trial", trial, "uid", p.UID)
					continue
				}
				na := nodes[fromA]
				if na == nil {
					klog.V(2).InfoS("random-swap: skip-eligible-with-unknown-fromA-node", "trial", trial, "uid", p.UID, "fromA", fromA)
					continue
				}

				// choose a different destination node
				destIdx := rng.Intn(maxInt(1, len(order)))
				for tries := 0; tries < len(order); tries++ {
					dst := order[(destIdx+tries)%len(order)]
					if dst.Name == fromA {
						continue
					}
					da := delta[fromA]
					db := delta[dst.Name]

					if action == 0 {
						// DIRECT MOVE
						needCPU, needMem := p.CPUm, p.MemBytes
						availCPU := dst.FreeCPUm + db.cpu
						availMem := dst.FreeMemBytes + db.mem
						if availCPU >= needCPU && availMem >= needMem {
							klog.V(3).InfoS("random-swap: direct-ok", "trial", trial,
								"uid", p.UID, "from", fromA, "to", dst.Name,
								"needCPU", needCPU, "needMem", needMem, "availCPU", availCPU, "availMem", availMem,
							)
							planMoves = append(planMoves, moveLite{UID: p.UID, From: fromA, To: dst.Name})
							delta[fromA] = deltaDM{cpu: da.cpu + p.CPUm, mem: da.mem + p.MemBytes}
							delta[dst.Name] = deltaDM{cpu: db.cpu - p.CPUm, mem: db.mem - p.MemBytes}
							finalLoc[p.UID] = dst.Name
							chosenUID[p.UID] = struct{}{}
							return true
						}
						klog.V(3).InfoS("random-swap: direct-reject", "trial", trial, "uid", p.UID, "from", fromA, "to", dst.Name,
							"needCPU", needCPU, "needMem", needMem, "availCPU", availCPU, "availMem", availMem,
						)
					} else {
						// SWAP
						candsQ := make([]*pLite, 0, len(dst.Pods))
						for _, q := range dst.Pods {
							if !isMovable(q) {
								continue
							}
							if _, seen := chosenUID[q.UID]; seen {
								continue
							}
							if q.Node == "" {
								continue
							}
							if loc := finalLoc[q.UID]; loc == "" {
								continue
							}
							candsQ = append(candsQ, q)
						}
						// prefer already-moved; then lower priority; then bigger pods
						sort.SliceStable(candsQ, func(i, j int) bool {
							_, pi := preferUIDs[candsQ[i].UID]
							_, pj := preferUIDs[candsQ[j].UID]
							if pi != pj {
								return pi
							}
							if candsQ[i].Priority != candsQ[j].Priority {
								return candsQ[i].Priority < candsQ[j].Priority
							}
							si := candsQ[i].CPUm * candsQ[i].MemBytes
							sj := candsQ[j].CPUm * candsQ[j].MemBytes
							if si != sj {
								return si > sj
							}
							return candsQ[i].UID < candsQ[j].UID
						})
						if len(candsQ) == 0 {
							klog.V(3).InfoS("random-swap: swap-reject-no-q", "trial", trial, "dst", dst.Name)
							continue
						}
						q := candsQ[rng.Intn(len(candsQ))]
						fromB := finalLoc[q.UID]
						if fromB == "" {
							klog.V(2).InfoS("random-swap: skip-swap-without-valid-fromB", "trial", trial, "q", q.UID)
							continue
						}
						nbFrom := nodes[fromB]
						if nbFrom == nil {
							klog.V(2).InfoS("random-swap: skip-swap-with-unknown-fromB-node", "trial", trial, "q", q.UID, "fromB", fromB)
							continue
						}

						finalA_CPU := (na.FreeCPUm + da.cpu) + p.CPUm - q.CPUm
						finalA_MEM := (na.FreeMemBytes + da.mem) + p.MemBytes - q.MemBytes
						finalB_CPU := (dst.FreeCPUm + db.cpu) + q.CPUm - p.CPUm
						finalB_MEM := (dst.FreeMemBytes + db.mem) + q.MemBytes - p.MemBytes

						if finalA_CPU >= 0 && finalA_MEM >= 0 && finalB_CPU >= 0 && finalB_MEM >= 0 {
							klog.V(3).InfoS("random-swap: swap-ok", "trial", trial,
								"p", p.UID, "q", q.UID, "A", fromA, "B", dst.Name,
								"finalA_CPU", finalA_CPU, "finalA_MEM", finalA_MEM,
								"finalB_CPU", finalB_CPU, "finalB_MEM", finalB_MEM,
							)
							planMoves = append(planMoves,
								moveLite{UID: p.UID, From: fromA, To: dst.Name},
								moveLite{UID: q.UID, From: fromB, To: fromA},
							)
							delta[fromA] = deltaDM{cpu: da.cpu + p.CPUm - q.CPUm, mem: da.mem + p.MemBytes - q.MemBytes}
							delta[dst.Name] = deltaDM{cpu: db.cpu + q.CPUm - p.CPUm, mem: db.mem + q.MemBytes - p.MemBytes}
							finalLoc[p.UID] = dst.Name
							finalLoc[q.UID] = fromA
							chosenUID[p.UID] = struct{}{}
							chosenUID[q.UID] = struct{}{}
							return true
						}
						klog.V(3).InfoS("random-swap: swap-reject",
							"trial", trial, "p", p.UID, "q", q.UID, "A", fromA, "B", dst.Name,
							"finalA_CPU", finalA_CPU, "finalA_MEM", finalA_MEM,
							"finalB_CPU", finalB_CPU, "finalB_MEM", finalB_MEM,
						)
					}
				} // for each dst
				// Try a different p/action again
			} // attempts
			return false
		}

		for len(planMoves) < SolverSwapMaxMovesPerPlan {
			if !tryStep() {
				break
			}

			// After each change, see if ALL pendingPods can be placed on the residuals.
			if assign, ok := checkBatchFitAfterDeltas(delta); ok {
				logResiduals("assignment-found", order, delta)
				fd := finalDestFromFinalLoc(finalLoc, orig)
				coalesced := squashMoves(fd, orig)

				// Validate endpoints exist
				valid := true
				for i, mv := range coalesced {
					if nodes[mv.From] == nil || nodes[mv.To] == nil {
						klog.V(2).InfoS("random-swap: drop-coalesced-move-with-unknown-node",
							"i", i, "from", mv.From, "to", mv.To)
						valid = false
						break
					}
				}
				if !valid {
					continue
				}

				sig := signatureFromMovesPlusAssign(coalesced, assign)
				if _, seen := triedPlans[sig]; seen {
					stats.dedupSkips++
					klog.V(2).InfoS("random-swap: dedup-skip", "moves", len(coalesced), "assigned", len(assign))
				} else {
					triedPlans[sig] = struct{}{}
					if len(coalesced) < bestCount {
						bestCount = len(coalesced)
						bestMoves = coalesced
						bestAssign = assign
						found = true
						stats.improvements++
						klog.InfoS("random-swap: improved-plan", "moves", bestCount, "assigned", len(assign))
						if bestCount <= 1 {
							return bestMoves, bestAssign, found, stats
						}
					}
				}
			} else {
				klog.V(3).InfoS("random-swap: batch-fit-failed")
				logResiduals("assignment-failed", order, delta)
			}

			// If we already have a plan with X moves, no point extending this trial beyond X.
			if found && len(planMoves) >= bestCount {
				klog.V(2).InfoS("random-swap: stop-extending-trial", "currentPlanMoves", len(planMoves), "bestMoves", bestCount)
				break
			}
		}

		// Mark the attempted plan (even unsuccessful) to avoid repeating
		fd := map[string]string{}
		for uid, from := range orig {
			if to := finalLoc[uid]; to != "" && to != from {
				fd[uid] = to
			}
		}
		coalesced := squashMoves(fd, orig)
		sig := signatureFromMovesPlusAssign(coalesced, map[string]string{})
		triedPlans[sig] = struct{}{}
	}

	return bestMoves, bestAssign, found, stats
}

//
// ========================= Public entry points =========================
//

// --- Logging helpers ---
// Small pretty-printer for residual capacity per node.
func logResiduals(prefix string, order []*nLite, delta map[string]deltaDM) {
	rows := make([]interface{}, 0, len(order)*4+2)
	rows = append(rows, "prefix", prefix)
	for _, n := range order {
		d := delta[n.Name]
		rows = append(rows,
			n.Name+"_cpu", n.FreeCPUm+d.cpu,
			n.Name+"_mem", n.FreeMemBytes+d.mem,
		)
	}
	klog.V(2).InfoS("residuals", rows...)
}

type deltaDM struct{ cpu, mem int64 }
