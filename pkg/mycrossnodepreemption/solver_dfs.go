// pkg/mycrossnodepreemption/solver_chain.go
package mycrossnodepreemption

import (
	"fmt"
	"math"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

// ========================= Tunables & types =========================

type chainCfg struct {
	MaxDepth          int
	MaxVictimsPerNode int
	MaxDestsPerLevel  int
	MaxTotalMoves     int // NEW: cap total unique pod moves in a plan
}

var defaultChainCfg = chainCfg{
	MaxDepth:          5,
	MaxVictimsPerNode: 0,
	MaxDestsPerLevel:  0,
	MaxTotalMoves:     5,
}

type pLite struct {
	UID       string
	CPUm      int64
	MemBytes  int64
	Priority  int32
	Protected bool
	Node      string // "" if pending
	origNode  string // snapshot of original node for coalescing
}

type nLite struct {
	Name    string
	CapCPU  int64
	CapMem  int64
	FreeCPU int64
	FreeMem int64
	Pods    map[string]*pLite
}

type dfsStats struct {
	traceID    string
	maxDepth   int
	visited    int64 // total dfs calls
	edgesTried int64 // total (victim,dest) edges explored

	// counts by node
	victimsListed map[string]int // how many candidate victims we enumerated on needOn
	destsListed   map[string]int // how many candidate dests we enumerated per expansion on needOn
	memoHits      int64
	prunesNoVic   int64
	prunesNoFit   int64
	forbidRootHit int64

	// for final per-target recap
	perTarget map[string]*perTargetStats
}

type perTargetStats struct {
	root      string
	attempts  int64
	successes int64
	memoHits  int64
	edges     int64
	maxDepth  int
}

func (n *nLite) fits(cpu, mem int64) bool { return n.FreeCPU >= cpu && n.FreeMem >= mem }
func (n *nLite) add(p *pLite) {
	n.FreeCPU -= p.CPUm
	n.FreeMem -= p.MemBytes
	n.Pods[p.UID] = p
	p.Node = n.Name
}
func (n *nLite) remove(p *pLite) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.FreeCPU += p.CPUm
		n.FreeMem += p.MemBytes
		p.Node = ""
	}
}

type moveLite struct{ UID, From, To string }

// add near the other top-level types
type resvDelta struct{ cpu, mem int64 }

// ---- Hoisted: used by dfsFreeNode/markFail ----
type memoKey struct {
	node     string
	cpu, mem int64
	prio     int32
	pathSig  string
	root     string
	freedCPU int64
	freedMem int64
}

// ============================= Entry ==============================

func runSolverDfs(in SolverInput) *SolverOutput {
	cfg := defaultChainCfg

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

	// -------------------- Step 1: direct fit only --------------------
	if to, ok := bestDirectFit(order, pre); ok {
		klog.InfoS("direct-fit", "preemptor", pre.UID, "to", to)
		nodes[to].add(pre)
		placements[pre.UID] = to
		return stableOutput("FEASIBLE", placements, evicts, in)
	}

	totalFree := func() (int64, int64) {
		var c, m int64
		for _, n := range order {
			c += n.FreeCPU
			m += n.FreeMem
		}
		return c, m
	}

	failed := map[memoKey]bool{}

	targets := func() []*nLite {
		ts := make([]*nLite, len(order))
		copy(ts, order)
		sort.Slice(ts, func(i, j int) bool {
			diCPU := max64(0, pre.CPUm-ts[i].FreeCPU)
			djCPU := max64(0, pre.CPUm-ts[j].FreeCPU)
			if diCPU != djCPU {
				return diCPU < djCPU
			}
			diMem := max64(0, pre.MemBytes-ts[i].FreeMem)
			djMem := max64(0, pre.MemBytes-ts[j].FreeMem)
			if diMem != djMem {
				return diMem < djMem
			}
			return ts[i].Name < ts[j].Name
		})
		return ts
	}

	trace := fmt.Sprintf("%s-%d", pre.UID, time.Now().UnixNano())
	stats := &dfsStats{
		traceID:       trace,
		maxDepth:      cfg.MaxDepth,
		victimsListed: make(map[string]int),
		destsListed:   make(map[string]int),
		perTarget:     make(map[string]*perTargetStats),
	}

tryRelocate:

	klog.InfoS("relocate: start", "trace", trace, "preemptor", pre.UID, "needCPU", pre.CPUm, "needMem", pre.MemBytes)

	if cTot, mTot := totalFree(); cTot < pre.CPUm || mTot < pre.MemBytes {
		klog.InfoS("relocate: total free insufficient (skip dfs, go evict)", "trace", trace, "totalFreeCPU", cTot, "totalFreeMem", mTot)
		goto tryEvict
	}

	for _, t := range targets() {
		if t.fits(pre.CPUm, pre.MemBytes) {
			t.add(pre)
			placements[pre.UID] = t.Name
			return stableOutput("FEASIBLE", placements, evicts, in)
		}

		if stats.perTarget[t.Name] == nil {
			stats.perTarget[t.Name] = &perTargetStats{root: t.Name}
		}
		stats.perTarget[t.Name].attempts++

		finalDest := map[string]string{}
		inFlight := map[string]bool{}
		origNode := map[string]string{}
		for uid, p := range all {
			if p.Node != "" {
				origNode[uid] = p.Node
			}
		}
		reserve := map[string]resvDelta{}

		needCPU := max64(0, pre.CPUm-t.FreeCPU)
		needMem := max64(0, pre.MemBytes-t.FreeMem)

		var plan []moveLite
		freedCPU := int64(0)
		freedMem := int64(0)
		if dfsFreeNode(cfg, nodes, all,
			t.Name /*needOn*/, t.Name, /*rootTarget*/
			needCPU, needMem, pre.Priority, cfg.MaxDepth,
			reserve, inFlight, finalDest, failed, &plan,
			/* freedOnTargetCPU */ &freedCPU,
			/* freedOnTargetMem */ &freedMem,
			/* preemptor requirements on root */ pre.CPUm, pre.MemBytes,
			stats, // pass stats
		) {

			coalesced := squashMoves(finalDest, origNode)
			coCPU, coMem := freedOnNodeFromCoalesced(t.Name, coalesced, all)

			if !t.fits(pre.CPUm, pre.MemBytes) && len(coalesced) == 0 {
				klog.InfoS("reject-empty-plan-for-nonfitting-target", "target", t.Name)
				markFailWithFreed(
					failed, t.Name, needCPU, needMem, pre.Priority, inFlight,
					t.Name, /*root*/
					/*freed*/ coCPU, coMem,
				)
				continue
			}

			shortCPU := max64(0, pre.CPUm-t.FreeCPU)
			shortMem := max64(0, pre.MemBytes-t.FreeMem)
			if coCPU < shortCPU || coMem < shortMem {
				klog.InfoS("coalesced-free-insufficient",
					"target", t.Name, "coalescedFreedCPU", coCPU, "coalescedFreedMem", coMem,
					"shortCPU", shortCPU, "shortMem", shortMem,
					"preCPU", pre.CPUm, "preMem", pre.MemBytes, "freeCPU_now", t.FreeCPU, "freeMem_now", t.FreeMem)
				markFailWithFreed(
					failed, t.Name, needCPU, needMem, pre.Priority, inFlight,
					t.Name, /*root*/
					/*freed*/ coCPU, coMem,
				)
				continue
			}

			// verify final state with preemptor on t
			if !verifyCoalescedPlan(nodes, all, coalesced, pre, t.Name) {
				klog.InfoS("plan rejected by verifier", "target", t.Name)
				markFailWithFreed(
					failed, t.Name, needCPU, needMem, pre.Priority, inFlight,
					t.Name, /*root*/
					/*freed*/ coCPU, coMem,
				)
				continue
			}

			if applyTwoPhase(nodes, all, coalesced) {
				for _, mv := range coalesced {
					placements[mv.UID] = mv.To
				}
				t.add(pre)
				placements[pre.UID] = t.Name

				// paranoid post-apply check
				if !verifyCoalescedPlan(nodes, all, nil, nil, "") {
					klog.InfoS("post-apply verify failed unexpectedly")
					return stableOutput("INFEASIBLE", placements, evicts, in)
				}
				stats.perTarget[t.Name].successes++
				return stableOutput("FEASIBLE", placements, evicts, in)
			}
		}

		// update per-target snapshot after this attempt
		if pt := stats.perTarget[t.Name]; pt != nil {
			pt.edges = stats.edgesTried
		}
	}

	// If we reach here, all targets failed (no apply). Emit a coverage summary before eviction.
	klog.InfoS("relocate: all targets failed; summary",
		"trace", trace,
		"dfsVisited", stats.visited, "edgesTried", stats.edgesTried,
		"memoHits", stats.memoHits, "prunesNoVictims", stats.prunesNoVic,
		"prunesNoFit", stats.prunesNoFit, "forbidRootHit", stats.forbidRootHit,
		"perTarget", func() map[string]any {
			m := map[string]any{}
			for k, v := range stats.perTarget {
				m[k] = map[string]any{
					"attempts":          v.attempts,
					"successes":         v.successes,
					"memoHitsObserved":  v.memoHits,
					"edgesExplored":     v.edges,
					"maxDepthReached":   v.maxDepth,
					"victimsEnumerated": stats.victimsListed[k],
					"destsEnumerated":   stats.destsListed[k],
				}
			}
			return m
		}(),
	)

tryEvict:

	if v, on := pickEvictionThatEnablesFit(order, pre); v != nil {
		on.remove(v)
		evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
		klog.InfoS("evict-one-and-retry", "victim", v.UID, "from", on.Name)
		goto tryRelocate
	}

	return stableOutput("INFEASIBLE", placements, evicts, in)
}

// ========================= DFS relocation core =========================

func dfsFreeNode(
	cfg chainCfg,
	nodes map[string]*nLite,
	all map[string]*pLite,
	needOn string,
	rootTarget string,
	needCPU, needMem int64,
	prioLimit int32,
	depthLeft int,
	reserve map[string]resvDelta,
	inFlight map[string]bool,
	finalDest map[string]string,
	failed map[memoKey]bool,
	plan *[]moveLite,
	freedOnTargetCPU *int64, // NEW
	freedOnTargetMem *int64, // NEW
	preCPU int64, // NEW: preemptor CPU need
	preMem int64, // NEW: preemptor Mem need
	stats *dfsStats, // NEW: instrumentation
) bool {
	uniqueMoves := func(fd map[string]string) int { return len(fd) }

	if depthLeft < 0 {
		return false
	}

	// Enter log (shows decreasing depth)
	if stats != nil {
		stats.visited++
		if pt := stats.perTarget[rootTarget]; pt != nil {
			d := cfg.MaxDepth - depthLeft
			if d > pt.maxDepth {
				pt.maxDepth = d
			}
		}
		klog.V(V2).InfoS("dfs: enter",
			"trace", stats.traceID,
			"needOn", needOn, "root", rootTarget,
			"needCPU", needCPU, "needMem", needMem,
			"depthLeft", depthLeft,
			"path", compactPathSig(inFlight),
		)
	}

	// base success check
	if enoughWithResv(nodes[needOn], needCPU, needMem, reserve) {
		if needOn == rootTarget {
			root := nodes[rootTarget]
			shortCPU := max64(0, preCPU-root.FreeCPU)
			shortMem := max64(0, preMem-root.FreeMem)
			if *freedOnTargetCPU >= shortCPU && *freedOnTargetMem >= shortMem {
				return true
			}
			klog.V(V2).InfoS("dfs: reservations meet needOn, but net freed on root insufficient after coalescing",
				"root", rootTarget, "freedCPU", *freedOnTargetCPU, "freedMem", *freedOnTargetMem,
				"shortCPU", shortCPU, "shortMem", shortMem,
				"preCPU", preCPU, "preMem", preMem, "rootFreeCPU", root.FreeCPU, "rootFreeMem", root.FreeMem)
			// keep searching
		} else {
			return true
		}
	}

	mk := memoKey{
		node: needOn, cpu: needCPU, mem: needMem, prio: prioLimit, pathSig: compactPathSig(inFlight),
	}
	if needOn == rootTarget {
		mk.root = rootTarget
		mk.freedCPU = *freedOnTargetCPU
		mk.freedMem = *freedOnTargetMem
	}
	if failed[mk] {
		if stats != nil {
			stats.memoHits++
			if pt := stats.perTarget[rootTarget]; pt != nil {
				pt.memoHits++
			}
		}
		klog.V(V2).InfoS("dfs: prune by memo",
			"trace", traceID(stats),
			"needOn", needOn, "needCPU", needCPU, "needMem", needMem,
			"freedCPU", *freedOnTargetCPU, "freedMem", *freedOnTargetMem,
			"path", mk.pathSig,
		)
		return false
	}

	vics := eligibleVictimsSorted(nodes[needOn], prioLimit, cfg.MaxVictimsPerNode, needCPU, needMem)
	if stats != nil {
		stats.victimsListed[needOn] = maxInt(stats.victimsListed[needOn], len(vics))
	}
	if len(vics) == 0 {
		if stats != nil {
			stats.prunesNoVic++
		}
		failed[mk] = true
		klog.V(V2).InfoS("dfs: prune (no eligible victims)",
			"trace", traceID(stats),
			"needOn", needOn)
		return false
	}
	dests := destsByFree(nodes, cfg.MaxDestsPerLevel)
	if stats != nil {
		stats.destsListed[needOn] = maxInt(stats.destsListed[needOn], len(dests))
	}

	// One-shot dump of the enumeration (verbose)
	ids := make([]string, 0, len(vics))
	for _, vv := range vics {
		ids = append(ids, fmt.Sprintf("%s(pri=%d,cpu=%d,mem=%d)", vv.UID, vv.Priority, vv.CPUm, vv.MemBytes))
	}
	dnNames := make([]string, 0, len(dests))
	for _, dnn := range dests {
		dnNames = append(dnNames, fmt.Sprintf("%s(freeCPU=%d,freeMem=%d)", dnn.Name, dnn.FreeCPU, dnn.FreeMem))
	}
	klog.V(V2).InfoS("dfs: enumerate",
		"trace", traceID(stats),
		"needOn", needOn, "victims", ids, "dests", dnNames)

	for _, v := range vics {
		if inFlight[v.UID] {
			continue
		}
		inFlight[v.UID] = true

		for _, dn := range dests {
			if dn.Name == needOn || dn.Pods[v.UID] != nil {
				continue
			}
			// Never move into the root target during relocation
			if dn.Name == rootTarget {
				if stats != nil {
					stats.forbidRootHit++
				}
				klog.V(V2).InfoS("dfs: forbid move into root",
					"trace", traceID(stats),
					"pod", v.UID, "to", dn.Name)
				continue
			}

			if cfg.MaxTotalMoves > 0 && uniqueMoves(finalDest) >= cfg.MaxTotalMoves {
				// don't add more; prune this branch
				continue
			}

			// Compute how much we must free on the destination to fit v
			needCPU2 := max64(0, v.CPUm-(dn.FreeCPU+reserve[dn.Name].cpu))
			needMem2 := max64(0, v.MemBytes-(dn.FreeMem+reserve[dn.Name].mem))

			if stats != nil {
				stats.edgesTried++
			}

			// Is this victim originally on the root target?
			orig := all[v.UID].origNode
			addsCPU, addsMem := int64(0), int64(0)
			if orig == rootTarget && needOn == rootTarget {
				addsCPU, addsMem = v.CPUm, v.MemBytes
			}

			// Tentatively reserve: free on needOn, consume on dn
			pushResv(reserve, needOn, +v.CPUm, +v.MemBytes)
			pushResv(reserve, dn.Name, -v.CPUm, -v.MemBytes)

			// Contribute to "freed on root"
			*freedOnTargetCPU += addsCPU
			*freedOnTargetMem += addsMem

			if needCPU2 > 0 || needMem2 > 0 {
				klog.V(V2).InfoS("dfs: dest short; try freeing destination",
					"trace", traceID(stats),
					"pod", v.UID, "dest", dn.Name,
					"needCPU2", needCPU2, "needMem2", needMem2,
					"depthLeft", depthLeft, "nextDepth", depthLeft-1)
			} else {
				klog.V(V2).InfoS("dfs: dest fits without freeing",
					"trace", traceID(stats),
					"pod", v.UID, "dest", dn.Name,
					"depthLeft", depthLeft)
			}

			ok := true
			if needCPU2 > 0 || needMem2 > 0 {
				ok = dfsFreeNode(cfg, nodes, all,
					dn.Name, rootTarget,
					needCPU2, needMem2, prioLimit, depthLeft-1,
					reserve, inFlight, finalDest, failed, plan,
					freedOnTargetCPU, freedOnTargetMem, preCPU, preMem, stats)
			}

			if ok {
				*plan = append(*plan, moveLite{UID: v.UID, From: needOn, To: dn.Name})
				// Will this be a "new" move for this UID?
				isNew := (finalDest[v.UID] == "" || finalDest[v.UID] == v.Node)
				if isNew && cfg.MaxTotalMoves > 0 && uniqueMoves(finalDest) >= cfg.MaxTotalMoves {
					// backtrack reservations before continue
					*freedOnTargetCPU -= addsCPU
					*freedOnTargetMem -= addsMem
					popResv(reserve, needOn, +v.CPUm, +v.MemBytes)
					popResv(reserve, dn.Name, -v.CPUm, -v.MemBytes)
					inFlight[v.UID] = false
					continue
				}
				finalDest[v.UID] = dn.Name

				done := false
				if enoughWithResv(nodes[needOn], needCPU, needMem, reserve) {
					if needOn != rootTarget {
						done = true
					} else {
						root := nodes[rootTarget]
						shortCPU := max64(0, preCPU-root.FreeCPU)
						shortMem := max64(0, preMem-root.FreeMem)
						if *freedOnTargetCPU >= shortCPU && *freedOnTargetMem >= shortMem {
							done = true
						} else {
							klog.V(V2).InfoS("dfs: reservations OK for needOn but net freed on root still insufficient",
								"root", rootTarget, "freedCPU", *freedOnTargetCPU, "freedMem", *freedOnTargetMem,
								"shortCPU", shortCPU, "shortMem", shortMem,
								"preCPU", preCPU, "preMem", preMem, "rootFreeCPU", root.FreeCPU, "rootFreeMem", root.FreeMem)
						}
					}
				}
				if !done {
					klog.V(V2).InfoS("dfs: need more, keep searching",
						"trace", traceID(stats),
						"needOn", needOn, "needCPU", needCPU, "needMem", needMem, "depthLeft", depthLeft)
					done = dfsFreeNode(cfg, nodes, all, needOn, rootTarget,
						needCPU, needMem, prioLimit, depthLeft,
						reserve, inFlight, finalDest, failed, plan,
						freedOnTargetCPU, freedOnTargetMem, preCPU, preMem, stats)
				}
				if done {
					inFlight[v.UID] = false
					return true
				}
			}

			// backtrack
			*freedOnTargetCPU -= addsCPU
			*freedOnTargetMem -= addsMem
			popResv(reserve, needOn, +v.CPUm, +v.MemBytes)
			popResv(reserve, dn.Name, -v.CPUm, -v.MemBytes)
			if len(*plan) > 0 {
				last := (*plan)[len(*plan)-1]
				if last.UID == v.UID && last.From == needOn && last.To == dn.Name {
					*plan = (*plan)[:len(*plan)-1]
				}
			}
			delete(finalDest, v.UID)
		}

		inFlight[v.UID] = false
	}

	failed[mk] = true
	return false
}

func freedOnNodeFromCoalesced(target string, moves []moveLite, all map[string]*pLite) (cpuFreed, memFreed int64) {
	var c, m int64
	for _, mv := range moves {
		p := all[mv.UID]
		if p != nil && p.origNode == target && mv.From == target && mv.To != target {
			c += p.CPUm
			m += p.MemBytes
		}
	}
	return c, m
}

// verifyCoalescedPlan computes final per-node free after applying `moves` and placing preemptor on target.
// It *does not* mutate nodes; it only reads current FreeCPU/FreeMem and returns ok + log details on fail.
func verifyCoalescedPlan(nodes map[string]*nLite, all map[string]*pLite, moves []moveLite, pre *pLite, target string) bool {
	type d struct{ cpu, mem int64 }
	per := map[string]d{}

	// apply moves deltas
	for _, mv := range moves {
		p := all[mv.UID]
		from, to := nodes[mv.From], nodes[mv.To]
		if p == nil || from == nil || to == nil {
			klog.InfoS("verify: invalid move endpoint", "uid", mv.UID, "from", mv.From, "to", mv.To)
			return false
		}
		df := per[from.Name]
		df.cpu += p.CPUm
		df.mem += p.MemBytes
		per[from.Name] = df
		dt := per[to.Name]
		dt.cpu -= p.CPUm
		dt.mem -= p.MemBytes
		per[to.Name] = dt
	}

	// add preemptor placement delta on target
	if target != "" && pre != nil {
		dt := per[target]
		dt.cpu -= pre.CPUm
		dt.mem -= pre.MemBytes
		per[target] = dt
	}

	// check all nodes
	ok := true
	for name, dd := range per {
		if n := nodes[name]; n != nil {
			fCPU := n.FreeCPU + dd.cpu
			fMEM := n.FreeMem + dd.mem
			if fCPU < 0 || fMEM < 0 {
				klog.InfoS("verify: final negative free", "node", name,
					"freeCPU_now", n.FreeCPU, "freeMem_now", n.FreeMem,
					"deltaCPU", dd.cpu, "deltaMem", dd.mem,
					"finalCPU", fCPU, "finalMem", fMEM)
				ok = false
			}
		}
	}
	if !ok {
		klog.InfoS("verify: move set (coalesced)", "count", len(moves))
		for i, mv := range moves {
			klog.InfoS("  mv", "i", i, "uid", mv.UID, "from", mv.From, "to", mv.To)
		}
	}
	return ok
}

// ========================= Helpers / scoring =========================

func bestDirectFit(order []*nLite, p *pLite) (string, bool) {
	best := ""
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.fits(p.CPUm, p.MemBytes) {
			cw := n.FreeCPU - p.CPUm
			mw := n.FreeMem - p.MemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && n.Name < best))) {
				best, bestCPUWaste, bestMEMWaste = n.Name, cw, mw
			}
		}
	}
	return best, best != ""
}

func eligibleVictimsSorted(n *nLite, prioLimit int32, capK int, needCPU, needMem int64) []*pLite {
	buf := make([]*pLite, 0, len(n.Pods))
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
		si := min64(buf[i].CPUm, needCPU)*wCPU + min64(buf[i].MemBytes, needMem)*wMem
		sj := min64(buf[j].CPUm, needCPU)*wCPU + min64(buf[j].MemBytes, needMem)*wMem
		if si != sj {
			return si > sj
		}
		if buf[i].Priority != buf[j].Priority {
			return buf[i].Priority < buf[j].Priority
		}
		if buf[i].CPUm != buf[j].CPUm {
			return buf[i].CPUm < buf[j].CPUm
		}
		if buf[i].MemBytes != buf[j].MemBytes {
			return buf[i].MemBytes < buf[j].MemBytes
		}
		return buf[i].UID < buf[j].UID
	})
	if capK > 0 && capK < len(buf) {
		return buf[:capK]
	}
	return buf
}

func destsByFree(nodes map[string]*nLite, capK int) []*nLite {
	ns := make([]*nLite, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].FreeCPU != ns[j].FreeCPU {
			return ns[i].FreeCPU > ns[j].FreeCPU
		}
		if ns[i].FreeMem != ns[j].FreeMem {
			return ns[i].FreeMem > ns[j].FreeMem
		}
		return ns[i].Name < ns[j].Name
	})
	if capK > 0 && capK < len(ns) {
		return ns[:capK]
	}
	return ns
}

// Reservations

func enoughWithResv(n *nLite, needCPU, needMem int64, reserve map[string]resvDelta) bool {
	d := reserve[n.Name]
	return (n.FreeCPU+d.cpu) >= needCPU && (n.FreeMem+d.mem) >= needMem
}

func pushResv(m map[string]resvDelta, node string, dcpu, dmem int64) {
	cur := m[node]
	cur.cpu += dcpu
	cur.mem += dmem
	m[node] = cur
}
func popResv(m map[string]resvDelta, node string, dcpu, dmem int64) {
	cur := m[node]
	cur.cpu -= dcpu
	cur.mem -= dmem
	m[node] = cur
}

func compactPathSig(inFlight map[string]bool) string {
	if len(inFlight) == 0 {
		return "-"
	}
	ids := make([]string, 0, len(inFlight))
	for uid, on := range inFlight {
		if on {
			ids = append(ids, uid)
		}
	}
	sort.Strings(ids)
	// Increase cap to reduce accidental collisions:
	if len(ids) > 32 {
		ids = ids[:32]
	}
	out := ""
	for _, s := range ids {
		out += s + ","
	}
	return out
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

func applyTwoPhase(nodes map[string]*nLite, all map[string]*pLite, moves []moveLite) bool {
	if len(moves) == 0 {
		return true
	}

	type d struct{ cpu, mem int64 }
	per := map[string]d{}
	for _, mv := range moves {
		p := all[mv.UID]
		from, to := nodes[mv.From], nodes[mv.To]
		if p == nil || from == nil || to == nil {
			klog.InfoS("apply: invalid endpoint", "uid", mv.UID, "from", mv.From, "to", mv.To)
			return false
		}
		df := per[from.Name]
		df.cpu += p.CPUm
		df.mem += p.MemBytes
		per[from.Name] = df
		dt := per[to.Name]
		dt.cpu -= p.CPUm
		dt.mem -= p.MemBytes
		per[to.Name] = dt
	}
	for name, dd := range per {
		if n := nodes[name]; n != nil {
			if n.FreeCPU+dd.cpu < 0 || n.FreeMem+dd.mem < 0 {
				klog.InfoS("apply: reject, final negative free", "node", name,
					"freeCPU_now", n.FreeCPU, "freeMem_now", n.FreeMem,
					"deltaCPU", dd.cpu, "deltaMem", dd.mem,
					"finalCPU", n.FreeCPU+dd.cpu, "finalMem", n.FreeMem+dd.mem)
				for i, mv := range moves {
					klog.InfoS("  mv", "i", i, "uid", mv.UID, "from", mv.From, "to", mv.To)
				}
				return false
			}
		}
	}

	// Step 1: Remove all moved pods from their sources
	for _, mv := range moves {
		p := all[mv.UID]
		if n := nodes[mv.From]; n != nil && n.Pods[p.UID] != nil {
			n.remove(p)
		}
	}

	// Step 2: Then add them to their final destinations
	for _, mv := range moves {
		p := all[mv.UID]
		if n := nodes[mv.To]; n != nil {
			if !n.fits(p.CPUm, p.MemBytes) {
				klog.InfoS("apply: reject, does not fit on destination",
					"uid", p.UID, "to", n.Name, "freeCPU_now", n.FreeCPU, "freeMem_now", n.FreeMem,
					"needCPU", p.CPUm, "needMem", p.MemBytes)
				return false
			}
			n.add(p)
		}
	}
	return true
}

func pickEvictionThatEnablesFit(order []*nLite, pre *pLite) (*pLite, *nLite) {
	tightCPU := pre.CPUm >= pre.MemBytes
	type cand struct {
		v  *pLite
		on *nLite
	}
	cands := make([]cand, 0, 64)
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Protected || q.Priority >= pre.Priority {
				continue
			}
			if n.FreeCPU+q.CPUm >= pre.CPUm && n.FreeMem+q.MemBytes >= pre.MemBytes {
				cands = append(cands, cand{v: q, on: n})
			}
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		vi, vj := cands[i].v, cands[j].v
		if vi.Priority != vj.Priority {
			return vi.Priority < vj.Priority
		}
		if tightCPU && vi.CPUm != vj.CPUm {
			return vi.CPUm > vj.CPUm
		}
		if !tightCPU && vi.MemBytes != vj.MemBytes {
			return vi.MemBytes > vj.MemBytes
		}
		if tightCPU {
			if vi.MemBytes != vj.MemBytes {
				return vi.MemBytes > vj.MemBytes
			}
		} else {
			if vi.CPUm != vj.CPUm {
				return vi.CPUm > vj.CPUm
			}
		}
		if cands[i].on.Name != cands[j].on.Name {
			return cands[i].on.Name < cands[j].on.Name
		}
		return vi.UID < vj.UID
	})
	return cands[0].v, cands[0].on
}

func stableOutput(status string, placements map[string]string, evicts []Placement, in SolverInput) *SolverOutput {
	uids := make([]string, 0, len(placements))
	for uid := range placements {
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	lookup := func(uid string) Pod {
		if in.Preemptor != nil && in.Preemptor.UID == uid {
			return Pod{UID: uid, Namespace: in.Preemptor.Namespace, Name: in.Preemptor.Name}
		}
		for i := range in.Pods {
			if in.Pods[i].UID == uid {
				return Pod{UID: uid, Namespace: in.Pods[i].Namespace, Name: in.Pods[i].Name}
			}
		}
		return Pod{UID: uid}
	}

	outPl := make([]NewPlacement, 0, len(uids))
	for _, uid := range uids {
		to := placements[uid]
		if to == "" {
			continue
		}
		outPl = append(outPl, NewPlacement{Pod: lookup(uid), ToNode: to})
	}
	return &SolverOutput{Status: status, Placements: outPl, Evictions: evicts}
}

// ============================ small utils ============================

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func markFailWithFreed(
	f map[memoKey]bool,
	node string, cpu, mem int64, prio int32,
	inFlight map[string]bool,
	root string, freedCPU, freedMem int64,
) {
	mk := memoKey{
		node: node, cpu: cpu, mem: mem, prio: prio,
		pathSig: compactPathSig(inFlight),
	}
	if root != "" && node == root {
		mk.root = root
		mk.freedCPU = freedCPU
		mk.freedMem = freedMem
	}
	f[mk] = true
}

// helper to print a safe trace id in logs
func traceID(stats *dfsStats) string {
	if stats == nil {
		return ""
	}
	return stats.traceID
}
