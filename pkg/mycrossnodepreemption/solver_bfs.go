package mycrossnodepreemption

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"time"

	"k8s.io/klog/v2"
)

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

// verifyCoalescedPlan computes final per-node free after applying `moves` and placing preemptor on target.
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

func pushResv(m map[string]resvDelta, node string, dcpu, dmem int64) {
	cur := m[node]
	cur.cpu += dcpu
	cur.mem += dmem
	m[node] = cur
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

// ============================= BFS =============================

func runSolverBfs(in SolverInput) *SolverOutput {
	cfg := defaultChainCfg

	// --- build nodes (unchanged) ---
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

	// --- build pods (unchanged) ---
	all := make(map[string]*pLite, len(in.Pods)+1)
	var pre *pLite
	if in.Preemptor != nil {
		pre = &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
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

	// direct fit first (unchanged)
	if to, ok := bestDirectFit(order, pre); ok {
		nodes[to].add(pre)
		placements[pre.UID] = to
		return stableOutput("FEASIBLE", placements, evicts, in)
	}

	// aggregate free check
	totalFree := func() (int64, int64) {
		var c, m int64
		for _, n := range order {
			c += n.FreeCPU
			m += n.FreeMem
		}
		return c, m
	}

	// targets sorted by smallest deficit (unchanged)
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

tryRelocate:
	klog.InfoS("bfs-relocate: start", "trace", trace, "preemptor", pre.UID, "needCPU", pre.CPUm, "needMem", pre.MemBytes)

	if cTot, mTot := totalFree(); cTot < pre.CPUm || mTot < pre.MemBytes {
		klog.InfoS("bfs-relocate: total free insufficient -> evict path", "totalFreeCPU", cTot, "totalFreeMem", mTot)
		goto tryEvict
	}

	for _, t := range targets() {
		if t.fits(pre.CPUm, pre.MemBytes) {
			t.add(pre)
			placements[pre.UID] = t.Name
			return stableOutput("FEASIBLE", placements, evicts, in)
		}

		needCPU := max64(0, pre.CPUm-t.FreeCPU)
		needMem := max64(0, pre.MemBytes-t.FreeMem)

		fd, ok := bfsFreeTargets(cfg, nodes, t.Name, needCPU, needMem, pre.Priority, trace)
		if !ok {
			continue
		}

		// coalesce to (from,to) pairs; verify and apply
		orig := map[string]string{}
		for uid, p := range all {
			if p.Node != "" {
				orig[uid] = p.Node
			}
		}
		coalesced := squashMoves(fd, orig)

		// Log the BFS result: raw chain steps and coalesced plan
		klog.InfoS("bfs: plan-found",
			"trace", trace,
			"target", t.Name,
			"coalescedMoves", len(coalesced),
		)
		for i, mv := range coalesced {
			klog.InfoS("bfs: coalesced-move", "i", i, "uid", mv.UID, "from", mv.From, "to", mv.To)
		}

		if !verifyCoalescedPlan(nodes, all, coalesced, pre, t.Name) {
			klog.V(V2).InfoS("bfs: verifier rejected plan", "target", t.Name, "moves", len(coalesced))
			continue
		}
		if !applyTwoPhase(nodes, all, coalesced) {
			klog.InfoS("bfs: applyTwoPhase rejected plan", "target", t.Name, "moves", len(coalesced))
			continue
		}
		for _, mv := range coalesced {
			placements[mv.UID] = mv.To
		}
		t.add(pre)
		placements[pre.UID] = t.Name

		// paranoid re-verify
		if !verifyCoalescedPlan(nodes, all, nil, nil, "") {
			klog.InfoS("bfs: post-apply verify failed unexpectedly")
			return stableOutput("INFEASIBLE", placements, evicts, in)
		}
		return stableOutput("FEASIBLE", placements, evicts, in)
	}

tryEvict:
	if v, on := pickEvictionThatEnablesFit(order, pre); v != nil {
		on.remove(v)
		evicts = append(evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})
		klog.InfoS("bfs: evict-one-and-retry", "victim", v.UID, "from", on.Name)
		goto tryRelocate
	}

	return stableOutput("INFEASIBLE", placements, evicts, in)
}

// ----- Pure BFS state for freeing capacity on a set of nodes -----

type deficit struct {
	node    string
	needCPU int64
	needMem int64
}

type bfsState struct {
	deficits  []deficit            // outstanding nodes to free (front is active)
	reserve   map[string]resvDelta // node -> reserved delta (+free on sources, -consumed on dests)
	finalDest map[string]string    // UID -> final destination node (each UID moved at most once)
	moves     []moveLite           // ordered move list (each step adds exactly one)
}

// helpers to clone small maps/slices (kept tiny by caps)
func cloneResv(m map[string]resvDelta) map[string]resvDelta {
	c := make(map[string]resvDelta, len(m))
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
func cloneMoves(mv []moveLite) []moveLite {
	c := make([]moveLite, len(mv))
	copy(c, mv)
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
	cfg chainCfg,
	nodes map[string]*nLite,
	rootTarget string,
	rootNeedCPU, rootNeedMem int64,
	prioLimit int32,
	trace string,
) (map[string]string, bool) {

	init := bfsState{
		deficits:  []deficit{{node: rootTarget, needCPU: rootNeedCPU, needMem: rootNeedMem}},
		reserve:   map[string]resvDelta{},
		finalDest: map[string]string{},
		moves:     nil,
	}

	front := []bfsState{init}
	initKey := sig(init.deficits, init.finalDest, init.reserve)
	visited := map[string]bool{initKey: true}

	// -------- instrumentation (aggregate; logged once on exit) --------
	totalExpanded := 0
	totalEnqueued := 0
	totalEdgesTried := 0
	prunedVisited := 0
	prunedRootHit := 0
	prunedSelfLoop := 0
	prunedCapMoves := 0 // kept in case you reintroduce a cap
	prunedNoFit := 0

	uniqVictimsEnum := map[string]bool{} // across all depths
	uniqVictimsUsed := map[string]bool{} // across all depths
	maxFrontier := 0

	goalDepth := -1
	foundRawSteps := 0
	foundChainVictims := 0 // unique UIDs with a finalDest at the goal

	defer func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		klog.InfoS("bfs: summary",
			"trace", trace,
			"ok", goalDepth >= 0,
			"goalDepth", goalDepth,
			"expanded", totalExpanded,
			"enqueued", totalEnqueued,
			"visited", len(visited),
			"edgesTried", totalEdgesTried,
			"prunedVisited", prunedVisited,
			"prunedRootHit", prunedRootHit,
			"prunedSelfLoop", prunedSelfLoop,
			"prunedCapMoves", prunedCapMoves,
			"prunedNoFit", prunedNoFit,
			"uniqVictimsEnumerated", len(uniqVictimsEnum),
			"uniqVictimsUsed", len(uniqVictimsUsed),
			"frontierMax", maxFrontier,
			"rawSteps", foundRawSteps,
			"chainVictims", foundChainVictims,
			"heapAlloc", ms.HeapAlloc,
			"heapInuse", ms.HeapInuse,
		)
	}()

	for depth := 0; depth <= cfg.MaxDepth; depth++ {
		// goal check within this layer
		for _, st := range front {
			if len(st.deficits) == 0 {
				goalDepth = depth
				foundRawSteps = len(st.moves)
				foundChainVictims = len(st.finalDest)
				return st.finalDest, true
			}
		}
		if depth == cfg.MaxDepth {
			break
		}

		totalExpanded += len(front)

		next := make([]bfsState, 0, len(front)*4)
		layerVictimsEnum := map[string]bool{}
		layerVictimsUsed := map[string]bool{}

		for _, st := range front {
			if len(st.deficits) == 0 {
				continue
			}
			need := st.deficits[0]

			// enumerate victims on active deficit node
			vics := eligibleVictimsSorted(
				nodes[need.node],
				prioLimit,
				cfg.MaxVictimsPerNode,
				need.needCPU,
				need.needMem,
			)
			for _, v := range vics {
				layerVictimsEnum[v.UID] = true
			}
			if len(vics) == 0 {
				continue
			}

			// candidate destinations
			dests := destsByFree(nodes, cfg.MaxDestsPerLevel)

			for _, v := range vics {
				if st.finalDest[v.UID] != "" {
					continue // each UID at most once
				}
				for _, dn := range dests {
					totalEdgesTried++

					if dn.Name == need.node {
						prunedSelfLoop++
						continue
					}
					if dn.Pods[v.UID] != nil {
						prunedSelfLoop++
						continue
					}
					if dn.Name == rootTarget {
						prunedRootHit++
						continue // don't consume root freed space
					}
					// (If you keep a MaxTotalMoves cap, check it here and bump prunedCapMoves)

					// destination shortage under current reserve
					dstRes := st.reserve[dn.Name]
					needCPU2 := max64(0, v.CPUm-(dn.FreeCPU+dstRes.cpu))
					needMem2 := max64(0, v.MemBytes-(dn.FreeMem+dstRes.mem))

					// progress rule (don't make a worse deficit than we relieve)
					freedCPU := min64(v.CPUm, need.needCPU)
					freedMem := min64(v.MemBytes, need.needMem)
					if needCPU2 > freedCPU || needMem2 > freedMem {
						prunedNoFit++
						continue
					}

					// successor state (adds exactly one move)
					ns := bfsState{
						deficits:  cloneDeficits(st.deficits),
						reserve:   cloneResv(st.reserve),
						finalDest: cloneFD(st.finalDest),
						moves:     cloneMoves(st.moves),
					}

					// reservations from this move
					pushResv(ns.reserve, need.node, +v.CPUm, +v.MemBytes)
					pushResv(ns.reserve, dn.Name, -v.CPUm, -v.MemBytes)

					// reduce front deficit
					remCPU := max64(0, need.needCPU-v.CPUm)
					remMem := max64(0, need.needMem-v.MemBytes)
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

					ns.moves = append(ns.moves, moveLite{UID: v.UID, From: need.node, To: dn.Name})
					ns.finalDest[v.UID] = dn.Name

					key := sig(ns.deficits, ns.finalDest, ns.reserve)
					if visited[key] {
						prunedVisited++
						continue
					}
					visited[key] = true
					next = append(next, ns)
					layerVictimsUsed[v.UID] = true
				}
			}
		}

		// update aggregates for the layer
		if len(next) > maxFrontier {
			maxFrontier = len(next)
		}
		totalEnqueued += len(next)
		for uid := range layerVictimsEnum {
			uniqVictimsEnum[uid] = true
		}
		for uid := range layerVictimsUsed {
			uniqVictimsUsed[uid] = true
		}

		front = next
	}

	// no plan found
	return nil, false
}

// add this helper
func sigResv(r map[string]resvDelta) string {
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
func sig(defs []deficit, fd map[string]string, res map[string]resvDelta) string {
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
