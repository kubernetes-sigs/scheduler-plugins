package mycrossnodepreemption

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

//
// ========================= Tunables & types =========================
//

type swapCfg struct {
	// Maximum number of pod moves allowed in one random plan (a swap counts as 2).
	MaxMovesPerPlan int
	// Maximum number of random swap plans to try before we fall back to evicting a pod.
	MaxSwapTrials int
}

var defaultSwapCfg = swapCfg{
	MaxMovesPerPlan: 6,
	MaxSwapTrials:   1000,
}

type deltaDM struct{ cpu, mem int64 }

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
			n.Name+"_cpu", n.FreeCPU+d.cpu,
			n.Name+"_mem", n.FreeMem+d.mem,
		)
	}
	klog.V(2).InfoS("residuals", rows...)
}

func sumFree(order []*nLite) (cpu, mem int64) {
	for _, n := range order {
		cpu += n.FreeCPU
		mem += n.FreeMem
	}
	return
}

func sumIncoming(incoming []*pLite) (cpu, mem int64) {
	for _, p := range incoming {
		cpu += p.CPUm
		mem += p.MemBytes
	}
	return
}

// runSolverSwap keeps your existing single-preemptor entry. Internally it calls the batch core.
func runSolverSwap(in SolverInput) *SolverOutput {
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

	return runSolverSwapBatchCore(in, nodes, order)
}

func deriveIncomingFromPending(in SolverInput) []*pLite {
	out := make([]*pLite, 0, len(in.Pods))
	for i := range in.Pods {
		sp := in.Pods[i]
		if sp.Where == "" { // pending => treat as incoming
			out = append(out, &pLite{
				UID:       sp.UID,
				CPUm:      sp.CPU_m,
				MemBytes:  sp.MemBytes,
				Priority:  sp.Priority,
				Protected: sp.Protected,
			})
		}
	}
	if in.Preemptor != nil {
		out = append(out, &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		})
	}
	return out
}

//
// ========================= Core (single + batch) =========================
//

func runSolverSwapBatchCore(
	in SolverInput,
	nodes map[string]*nLite,
	order []*nLite,
) *SolverOutput {
	cfg := defaultSwapCfg
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Build movable pods
	all := make(map[string]*pLite, len(in.Pods)+1)
	// Single preemptor -> incoming slice of len 1 (if present

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

	incoming := deriveIncomingFromPending(in)

	trace := time.Now().Format("150405.000000000")
	inCPU, inMem := sumIncoming(incoming)
	freeCPU, freeMem := sumFree(order)
	klog.InfoS("random-swap: start",
		"trace", trace,
		"incomingCount", len(incoming),
		"incomingCPU", inCPU, "incomingMem", inMem,
		"clusterFreeCPU", freeCPU, "clusterFreeMem", freeMem,
		"maxMovesPerPlan", cfg.MaxMovesPerPlan, "maxSwapTrials", cfg.MaxSwapTrials,
	)

	placements := make(map[string]string)
	var evicts []Placement

	if len(incoming) == 0 {
		klog.InfoS("random-swap: no incoming pods", "trace", trace)
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	// 0) Try direct greedy fit for ALL incoming preemptors.
	if assign, ok := greedyBatchDirectFit(order, incoming); ok {
		klog.InfoS("random-swap: direct-batch-fit success", "trace", trace)
		for _, p := range incoming {
			n := nodes[assign[p.UID]]
			n.add(p)
			placements[p.UID] = n.Name
		}
		return stableOutput("FEASIBLE", placements, evicts, in)
	}
	klog.InfoS("random-swap: direct-batch-fit failed, continuing to random search", "trace", trace)

	// 1) Random swap search; keep de-dup of tried plans (moves+assignment).
	triedPlans := map[string]struct{}{}

	for {
		bestMoves, bestAssign, found, stats := tryRandomSwapPlansBatch(nodes, all, order, incoming, cfg, rng, triedPlans, trace)
		klog.InfoS("random-swap: trial-summary",
			"trace", trace,
			"trials", stats.trials,
			"improvements", stats.improvements,
			"dedupSkips", stats.dedupSkips,
			"lastEligible", stats.lastEligible,
		)

		if found {
			// Verify & apply moves (no preemptor placed yet).
			if !verifyCoalescedPlan(nodes, all, bestMoves, nil, "") {
				klog.InfoS("random-swap: best plan failed verification, falling back to eviction",
					"trace", trace, "moves", len(bestMoves))
			} else if applyTwoPhase(nodes, all, bestMoves) {
				klog.InfoS("random-swap: applying moves", "trace", trace, "moves", len(bestMoves))
				for _, mv := range bestMoves {
					placements[mv.UID] = mv.To
				}
				for _, p := range incoming {
					target := bestAssign[p.UID]
					if n := nodes[target]; n != nil && n.fits(p.CPUm, p.MemBytes) {
						n.add(p)
						placements[p.UID] = n.Name
					} else {
						klog.InfoS("random-swap: target no longer fits after apply; trying bestDirectFit",
							"trace", trace, "preemptor", p.UID, "target", target)
						if to, ok := bestDirectFit(order, p); ok {
							nodes[to].add(p)
							placements[p.UID] = to
						} else {
							klog.InfoS("random-swap: cannot place even after moves; will evict", "trace", trace, "preemptor", p.UID)
							goto tryEvict
						}
					}
				}
				klog.InfoS("random-swap: success", "trace", trace, "placed", len(incoming), "moves", len(bestMoves))
				return stableOutput("FEASIBLE", placements, evicts, in)
			}
		} else {
			klog.InfoS("random-swap: no plan found this round", "trace", trace)
		}

	tryEvict:
		// 2) Eviction fallback
		v, on := pickLowestPriorityGlobalForBatch(order, incoming)
		if v == nil || on == nil {
			klog.InfoS("random-swap: infeasible (no eligible victim)", "trace", trace)
			return stableOutput("INFEASIBLE", placements, evicts, in)
		}
		on.remove(v)
		evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
		klog.InfoS("random-swap: evict-and-retry", "trace", trace, "victim", v.UID, "prio", v.Priority, "from", on.Name)

		// After eviction, quick greedy check again
		if assign, ok := greedyBatchDirectFit(order, incoming); ok {
			klog.InfoS("random-swap: direct-batch-fit success after eviction", "trace", trace)
			for _, p := range incoming {
				n := nodes[assign[p.UID]]
				n.add(p)
				placements[p.UID] = n.Name
			}
			return stableOutput("FEASIBLE", placements, evicts, in)
		}
		triedPlans = map[string]struct{}{} // state changed; reset de-dup
	}
}

// ========================= Batch fitting helpers =========================
//

// Greedy direct fit for a batch (Best-Fit-Decreasing).
func greedyBatchDirectFit(order []*nLite, incoming []*pLite) (map[string]string, bool) {
	type capDM struct{ cpu, mem int64 }

	// Sort by “size” to pack big ones first.
	cand := make([]*pLite, len(incoming))
	copy(cand, incoming)
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

	residual := make(map[string]capDM, len(order))
	for _, n := range order {
		residual[n.Name] = capDM{cpu: n.FreeCPU, mem: n.FreeMem}
	}

	assign := make(map[string]string, len(cand))
	for _, p := range cand {
		best := ""
		bestCPUWaste := int64(math.MaxInt64)
		bestMEMWaste := int64(math.MaxInt64)
		for _, n := range order {
			r := residual[n.Name]
			if r.cpu >= p.CPUm && r.mem >= p.MemBytes {
				wc := r.cpu - p.CPUm
				wm := r.mem - p.MemBytes
				if wc < bestCPUWaste || (wc == bestCPUWaste && (wm < bestMEMWaste || (wm == bestMEMWaste && n.Name < best))) {
					best, bestCPUWaste, bestMEMWaste = n.Name, wc, wm
				}
			}
		}
		if best == "" {
			return nil, false
		}
		assign[p.UID] = best
		r := residual[best]
		residual[best] = capDM{cpu: r.cpu - p.CPUm, mem: r.mem - p.MemBytes}
	}
	return assign, true
}

// Batch fit on current residuals (Free + delta), returns assignment for ALL incoming if possible.
func batchFitOnResidual(order []*nLite, delta map[string]deltaDM, incoming []*pLite) (map[string]string, bool) {
	type capDM struct{ cpu, mem int64 }

	// Compute residual capacity per node
	resid := make(map[string]capDM, len(order))
	for _, n := range order {
		d := delta[n.Name]
		resid[n.Name] = capDM{cpu: n.FreeCPU + d.cpu, mem: n.FreeMem + d.mem}
	}

	// Best-Fit-Decreasing on these residuals
	cand := make([]*pLite, len(incoming))
	copy(cand, incoming)
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
			klog.V(2).InfoS("batch-fit: cannot-place",
				"uid", p.UID, "needCPU", p.CPUm, "needMem", p.MemBytes)
			for _, n := range order {
				r := resid[n.Name]
				klog.V(2).InfoS("batch-fit: node-free",
					"node", n.Name, "cpu", r.cpu, "mem", r.mem)
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

func maxIncomingPriority(incoming []*pLite) int32 {
	var mx int32 = math.MinInt32
	for _, p := range incoming {
		if p != nil && p.Priority > mx {
			mx = p.Priority
		}
	}
	return mx
}

// Evict globally lowest-priority eligible pod for the batch (priority strictly lower than max incoming).
func pickLowestPriorityGlobalForBatch(order []*nLite, incoming []*pLite) (*pLite, *nLite) {
	allowPri := maxIncomingPriority(incoming)
	var bestPod *pLite
	var bestNode *nLite
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Protected {
				continue
			}
			if q.Priority >= allowPri {
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
	incoming []*pLite,
	cfg swapCfg,
	rng *rand.Rand,
	triedPlans map[string]struct{},
	trace string,
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
		return batchFitOnResidual(order, delta, incoming)
	}

	// Movability gate: allow moving pods up to the max incoming priority.
	allowPri := maxIncomingPriority(incoming)
	isMovable := func(p *pLite) bool {
		if p == nil || p.Node == "" || p.Protected {
			return false
		}
		return p.Priority <= allowPri
	}

	for trial := 0; trial < cfg.MaxSwapTrials; trial++ {
		stats.trials++
		orig := buildOrig()

		// Virtual state trackers for this trial
		delta := map[string]deltaDM{}      // per-node free deltas
		finalLoc := map[string]string{}    // virtual final location per UID
		chosenUID := map[string]struct{}{} // avoid picking same UID multiple times in plan
		for uid, from := range orig {
			finalLoc[uid] = from
		}

		planMoves := make([]moveLite, 0, cfg.MaxMovesPerPlan)

		tryStep := func() bool {
			// count eligible once at start of attempt
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
			stats.lastEligible = len(eligible)
			if len(eligible) == 0 {
				klog.V(2).InfoS("random-swap: no-eligible-movable-pods", "trace", trace, "trial", trial)
				return false
			}

			for attempt := 0; attempt < 16; attempt++ {
				action := rng.Intn(2) // 0=direct, 1=swap
				p := eligible[rng.Intn(len(eligible))]
				fromA := finalLoc[p.UID]

				// choose a different destination node
				destIdx := rng.Intn(maxInt(1, len(order)))
				triedDests := 0

				for tries := 0; tries < len(order); tries++ {
					dst := order[(destIdx+tries)%len(order)]
					if dst.Name == fromA {
						continue
					}
					triedDests++
					da := delta[fromA]
					db := delta[dst.Name]

					if action == 0 {
						needCPU, needMem := p.CPUm, p.MemBytes
						availCPU := dst.FreeCPU + db.cpu
						availMem := dst.FreeMem + db.mem
						if availCPU >= needCPU && availMem >= needMem {
							klog.V(3).InfoS("random-swap: direct-ok", "trace", trace, "trial", trial,
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
						klog.V(3).InfoS("random-swap: direct-reject", "trace", trace, "trial", trial,
							"uid", p.UID, "from", fromA, "to", dst.Name,
							"needCPU", needCPU, "needMem", needMem, "availCPU", availCPU, "availMem", availMem,
						)
					} else {
						// Swap
						candsQ := make([]*pLite, 0, len(dst.Pods))
						for _, q := range dst.Pods {
							if !isMovable(q) {
								continue
							}
							if _, seen := chosenUID[q.UID]; seen {
								continue
							}
							candsQ = append(candsQ, q)
						}
						if len(candsQ) == 0 {
							klog.V(3).InfoS("random-swap: swap-reject-no-q", "trace", trace, "trial", trial, "dst", dst.Name)
							continue
						}
						q := candsQ[rng.Intn(len(candsQ))]
						fromB := finalLoc[q.UID]

						finalA_CPU := (nodes[fromA].FreeCPU + da.cpu) + p.CPUm - q.CPUm
						finalA_MEM := (nodes[fromA].FreeMem + da.mem) + p.MemBytes - q.MemBytes
						finalB_CPU := (dst.FreeCPU + db.cpu) + q.CPUm - p.CPUm
						finalB_MEM := (dst.FreeMem + db.mem) + q.MemBytes - p.MemBytes

						if finalA_CPU >= 0 && finalA_MEM >= 0 && finalB_CPU >= 0 && finalB_MEM >= 0 {
							klog.V(3).InfoS("random-swap: swap-ok", "trace", trace, "trial", trial,
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
							"trace", trace, "trial", trial, "p", p.UID, "q", q.UID, "A", fromA, "B", dst.Name,
							"finalA_CPU", finalA_CPU, "finalA_MEM", finalA_MEM,
							"finalB_CPU", finalB_CPU, "finalB_MEM", finalB_MEM,
						)
					}
				}
				// Try a different p/action again
			}
			return false
		}

		for len(planMoves) < cfg.MaxMovesPerPlan {
			if !tryStep() {
				break
			}

			// After each change, see if ALL incoming can be placed on the residuals.
			if assign, ok := checkBatchFitAfterDeltas(delta); ok {
				logResiduals("assignment-found", order, delta)
				fd := finalDestFromFinalLoc(finalLoc, orig)
				coalesced := squashMoves(fd, orig)
				sig := signatureFromMovesPlusAssign(coalesced, assign)
				if _, seen := triedPlans[sig]; seen {
					stats.dedupSkips++
					klog.V(2).InfoS("random-swap: dedup-skip", "trace", trace, "moves", len(coalesced), "assigned", len(assign))
				} else {
					triedPlans[sig] = struct{}{}
					if len(coalesced) < bestCount {
						bestCount = len(coalesced)
						bestMoves = coalesced
						bestAssign = assign
						found = true
						stats.improvements++
						klog.InfoS("random-swap: improved-plan", "trace", trace, "moves", bestCount, "assigned", len(assign))
						if bestCount <= 1 {
							return bestMoves, bestAssign, found, stats
						}
					}
				}
			} else {
				klog.V(3).InfoS("random-swap: batch-fit-failed", "trace", trace)
				logResiduals("assignment-failed", order, delta)
			}

			// If we already have a plan with X moves, no point extending this trial beyond X.
			if found && len(planMoves) >= bestCount {
				klog.V(2).InfoS("random-swap: stop-extending-trial", "trace", trace, "currentPlanMoves", len(planMoves), "bestMoves", bestCount)
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
