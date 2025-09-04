// solver_fast.go

// while pod not placed AND there are still eviction candidates:
//     1. Direct Fit:
//         - For each node:
//             - If node fits pod, place pod and break.

//     2. Relocation Chain (No Evictions):
//         - If cluster free is enough:
//             - For up to K target nodes (smallest deficit):
//                 - If target fits pod, place pod and break.
//                 - Else, try to free room via BFS relocation chain:
//                     - Only move pods with priority ≤ pod.priority
//                     - Never evict (delete) pods
//                     - If chain found, execute moves, place pod, break.

//     3. Single-Eviction Fallback:
//         - For each node:
//             - If node is close to fitting pod:
//                 - Find lowest-priority non-protected pod with priority < pod.priority
//                 - If evicting it enables fit:
//                     - Evict pod, place preemptor, break.
//                     - Mark this pod as evicted for future attempts.

//     End of inner loop for this pod

//     If pod still not placed:
//         - Remove each pod evicted in this round from the system (do not try to evict again).
//         - Repeat the loop for this pod with updated state.

// If no more pods can be evicted for this pending pod, mark as infeasible.

package mycrossnodepreemption

import (
	"math"
	"sort"

	"k8s.io/klog/v2"
)

/* ============================= Tunables ============================= */

type fastConfig struct {
	TargetsToTry        int // <=0 unlimited
	VictimsPerLevel     int // <=0 unlimited
	DestsPerLevel       int // <=0 unlimited
	MaxIterationsPerPod int // <=0 unlimited
}

var defaultFast = fastConfig{
	TargetsToTry:        -1,
	VictimsPerLevel:     -1,
	DestsPerLevel:       -1,
	MaxIterationsPerPod: 10_000,
}

/* =========================== Light state =========================== */

type pState struct {
	UID       string
	CPUm      int64
	MemBytes  int64
	Priority  int32
	Protected bool
	Node      string // "" if pending
}

type nState struct {
	Name     string
	CapCPUm  int64
	CapMem   int64
	FreeCPUm int64
	FreeMem  int64
	Pods     map[string]*pState
}

func (n *nState) fits(cpu, mem int64) bool { return n.FreeCPUm >= cpu && n.FreeMem >= mem }
func (n *nState) add(p *pState) {
	n.FreeCPUm -= p.CPUm
	n.FreeMem -= p.MemBytes
	n.Pods[p.UID] = p
	p.Node = n.Name
}
func (n *nState) remove(p *pState) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.FreeCPUm += p.CPUm
		n.FreeMem += p.MemBytes
		p.Node = ""
	}
}

type move struct{ UID, From, To string }

type ctxFast struct {
	cfg     fastConfig
	nodes   map[string]*nState
	order   []*nState
	allPods map[string]*pState
	uidInfo map[string]Pod
}

/* ============================ Utilities ============================ */

func capK(K, n int) int {
	if K <= 0 || K > n {
		return n
	}
	return K
}
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

/* ============================= Entry =============================== */

func runSolverFast(in SolverInput) *SolverOutput {
	c := ctxFast{
		cfg:     defaultFast,
		nodes:   make(map[string]*nState, len(in.Nodes)),
		order:   make([]*nState, 0, len(in.Nodes)),
		allPods: make(map[string]*pState, len(in.Pods)+1),
		uidInfo: make(map[string]Pod, len(in.Pods)+1),
	}

	// Nodes
	for i := range in.Nodes {
		n := &nState{
			Name:     in.Nodes[i].Name,
			CapCPUm:  in.Nodes[i].CPUm,
			CapMem:   in.Nodes[i].MemBytes,
			FreeCPUm: in.Nodes[i].CPUm,
			FreeMem:  in.Nodes[i].MemBytes,
			Pods:     make(map[string]*pState, 64),
		}
		c.nodes[n.Name] = n
		c.order = append(c.order, n)
	}

	for _, n := range c.order {
		klog.V(V2).InfoS("Node snapshot",
			"node", n.Name,
			"capCPU_m", n.CapCPUm, "capMem_MiB", bytesToMiB(n.CapMem),
			"freeCPU_m", n.FreeCPUm, "freeMem_MiB", bytesToMiB(n.FreeMem),
			"pods", len(n.Pods))
	}

	// Pods
	var pending []*pState
	if in.Preemptor != nil {
		pre := &pState{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		}
		c.allPods[pre.UID] = pre
		pending = append(pending, pre)
		c.uidInfo[pre.UID] = Pod{
			UID:       in.Preemptor.UID,
			Namespace: in.Preemptor.Namespace,
			Name:      in.Preemptor.Name,
		}
	}
	for i := range in.Pods {
		sp := in.Pods[i]
		p := &pState{
			UID:       sp.UID,
			CPUm:      sp.CPU_m,
			MemBytes:  sp.MemBytes,
			Priority:  sp.Priority,
			Protected: sp.Protected,
			Node:      sp.Where,
		}
		c.allPods[p.UID] = p
		c.uidInfo[p.UID] = Pod{UID: sp.UID, Namespace: sp.Namespace, Name: sp.Name}
		if p.Node == "" {
			pending = append(pending, p)
		} else if n := c.nodes[p.Node]; n != nil {
			n.add(p)
		}
	}

	// Highest prio first; then larger CPU; then Mem.
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Priority != pending[j].Priority {
			return pending[i].Priority > pending[j].Priority
		}
		if pending[i].CPUm != pending[j].CPUm {
			return pending[i].CPUm > pending[j].CPUm
		}
		return pending[i].MemBytes > pending[j].MemBytes
	})

	placementsByUID := make(map[string]string) // final To per UID
	var evicted []Placement
	preUID := ""
	if in.Preemptor != nil {
		preUID = in.Preemptor.UID
	}

	for _, p := range pending {
		placeForPod(&c, p, placementsByUID, &evicted)
		if preUID != "" && p.UID == preUID {
			break // single-preemptor mode: stop once it's placed/attempted
		}
	}

	status := "FEASIBLE"
	if preUID != "" {
		if _, ok := placementsByUID[preUID]; !ok {
			status = "INFEASIBLE"
		}
	}

	// Stable, deterministic placements
	uids := make([]string, 0, len(placementsByUID))
	for uid := range placementsByUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	newPl := make([]NewPlacement, 0, len(uids))
	for _, uid := range uids {
		to := placementsByUID[uid]
		if to == "" {
			continue
		}
		info := c.uidInfo[uid]
		newPl = append(newPl, NewPlacement{Pod: info, ToNode: to})
	}

	return &SolverOutput{Status: status, Placements: newPl, Evictions: evicted}
}

/* ========================= Core placement flow ========================= */

// Iterative: Direct fit -> relocations -> single eviction (global policy) -> repeat
func placeForPod(c *ctxFast, p *pState, placements map[string]string, evicted *[]Placement) bool {
	budget := c.cfg.MaxIterationsPerPod

	for {
		// 1) Direct fit (best-waste)
		if to, ok := bestDirectFit(c.order, c.nodes, p); ok {
			n := c.nodes[to]
			n.add(p)
			placements[p.UID] = to
			klog.V(V2).InfoS("Direct fit", "pod", podName(c, p.UID), "to", to)
			return true
		}

		// 2) BFS relocations (no evictions) if cluster aggregate free is sufficient
		if cpuTot, memTot := totalFree(c.order); cpuTot >= p.CPUm && memTot >= p.MemBytes {
			klog.V(V2).InfoS("placeForPod: try targets",
				"pod", podName(c, p.UID),
				"targets", func() []string {
					xs := []string{}
					for _, n := range topKTargetsByDeficit(c.order, p, c.cfg.TargetsToTry) {
						xs = append(xs, n.Name)
					}
					return xs
				}())

			for _, t := range topKTargetsByDeficit(c.order, p, c.cfg.TargetsToTry) {
				// trivial fit after prior changes
				if t.fits(p.CPUm, p.MemBytes) {
					t.add(p)
					placements[p.UID] = t.Name
					return true
				}
				// Restrict relocations to pods with priority <= preemptor
				mvs, ok := bfsFreeTarget(c, t, p, int(p.Priority), &budget)
				if ok {
					recordMoves(mvs, placements)
					t.add(p)
					placements[p.UID] = t.Name
					for i, mv := range mvs {
						pm := c.allPods[mv.UID]
						klog.V(V2).InfoS("  move",
							"idx", i, "pod", podName(c, mv.UID),
							"from", mv.From, "to", mv.To,
							"cpu_m", pm.CPUm, "mem_MiB", bytesToMiB(pm.MemBytes))
					}
					return true
				}
				if budget <= 0 {
					klog.V(V2).InfoS("Budget exhausted while searching BFS relocations",
						"pod", podName(c, p.UID))
					break
				}
			}
		}

		// 3) Single eviction (global): strictly lower priority than preemptor,
		// choose globally lowest priority, tie-break by highest CPU, then highest Mem.
		// Does NOT require immediate fit; we loop back to step 1.
		if v, on := pickOneEvictionGlobal(c.order, p); v != nil && on != nil {
			on.remove(v)
			info, ok := c.uidInfo[v.UID]
			if !ok {
				info = Pod{UID: v.UID}
			}
			*evicted = append(*evicted, Placement{Pod: info, Node: on.Name})
			klog.V(V2).InfoS("Evicted one victim (global policy, progress)",
				"victim", podName(c, v.UID), "victimPri", v.Priority,
				"cpu_m", v.CPUm, "mem_MiB", bytesToMiB(v.MemBytes),
				"from", on.Name, "preemptor", podName(c, p.UID), "preemptorPri", p.Priority)
			// Loop back to step 1 with updated state.
			continue
		}

		// No direct fit, no relocation chain, and no eligible eviction left -> infeasible for this pod.
		return false
	}
}

// Evict exactly one strictly-lower-priority, non-protected victim globally.
// Selection order: lowest priority -> highest CPU -> highest Mem.
// Does not require that the eviction immediately enables a fit.
func pickOneEvictionGlobal(order []*nState, p *pState) (*pState, *nState) {
	var bestV *pState
	var bestN *nState
	// Priority ascending (lower first). For CPU/Mem we want highest, so init with minima.
	bestKey := struct {
		priority int32
		cpu, mem int64
	}{math.MaxInt32, math.MinInt64, math.MinInt64}

	for _, n := range order {
		for _, rp := range n.Pods {
			// Strictly lower priority than preemptor; never evict protected.
			if rp.Protected || rp.Priority >= p.Priority {
				continue
			}
			key := struct {
				priority int32
				cpu, mem int64
			}{rp.Priority, rp.CPUm, rp.MemBytes}

			// Compare by (priority asc) -> (cpu desc) -> (mem desc)
			better := false
			if key.priority < bestKey.priority {
				better = true
			} else if key.priority == bestKey.priority {
				if key.cpu > bestKey.cpu {
					better = true
				} else if key.cpu == bestKey.cpu && key.mem > bestKey.mem {
					better = true
				}
			}
			if better {
				bestV, bestN, bestKey = rp, n, key
			}
		}
	}
	return bestV, bestN
}

/* ============================ BFS relocation ============================ */

type bfsKey struct {
	needNode string // node that must end up hosting needUID
	needUID  string // pod that must move to free space for the previous state
}
type parentEdge struct {
	prev bfsKey
	mov  move
	ok   bool
}

func bfsFreeTarget(c *ctxFast, t *nState, p *pState, prioLimit int, budget *int) ([]move, bool) {
	klog.V(V2).InfoS("BFS start",
		"targetNode", t.Name,
		"preemptor", podName(c, p.UID),
		"needCPU_m", max64(0, p.CPUm-t.FreeCPUm),
		"needMem_MiB", bytesToMiB(max64(0, p.MemBytes-t.FreeMem)),
		"prioLimit", prioLimit)
	var acc []move
	for !t.fits(p.CPUm, p.MemBytes) {
		if !dec(budget) {
			return nil, false
		}
		needCPU := max64(0, p.CPUm-t.FreeCPUm)
		needMem := max64(0, p.MemBytes-t.FreeMem)

		vics := pickVictims(t, prioLimit, needCPU, needMem, c.cfg.VictimsPerLevel)
		if len(vics) == 0 {
			klog.V(V2).InfoS("BFS abort: no eligible victims", "targetNode", t.Name)
			return nil, false
		}

		var chain []move
		found := false
		for _, v0 := range vics {
			// Quick direct move for the selected victim v0 (keeps the "coverage" bias).
			if dst := bestDirectDest(c.nodes, v0); dst != "" && dst != t.Name {
				klog.V(V2).InfoS("BFS quick move",
					"pod", podName(c, v0.UID), "from", t.Name, "to", dst)
				chain = []move{{UID: v0.UID, From: t.Name, To: dst}}
				found = true
				break
			}
			// BFS relocation chain to free 't' for 'p' (no eviction). IMPORTANT:
			// Seed BFS with "need p on t", not with v0.
			if ch, ok := bfsOne(c, t, p, prioLimit, budget); ok {
				chain, found = ch, true
				break
			}
		}
		if !found {
			klog.V(V2).InfoS("BFS failed to free target", "targetNode", t.Name)
			return nil, false
		}
		if !applyTwoPhase(c, c.nodes, c.allPods, chain) {
			return nil, false
		}
		acc = append(acc, chain...)
	}
	return acc, true
}

func podName(c *ctxFast, uid string) string {
	if info, ok := c.uidInfo[uid]; ok {
		if info.Namespace != "" {
			return info.Namespace + "/" + info.Name
		}
		return info.Name
	}
	return uid
}

func bfsOne(c *ctxFast, t *nState, pre *pState, prioLimit int, budget *int) ([]move, bool) {
	start := bfsKey{needNode: t.Name, needUID: pre.UID}
	q := []bfsKey{start}
	par := map[bfsKey]parentEdge{}
	seen := map[bfsKey]bool{start: true}

	for len(q) > 0 {
		if !dec(budget) {
			klog.InfoS("BFS queue stop: budget exhausted")
			return nil, false
		}
		cur := q[0]
		q = q[1:]

		needNode := c.nodes[cur.needNode]
		if needNode == nil {
			continue
		}

		// During relocation (no eviction), allow moving any non-protected pod.
		// Priority constraints apply to evictions (step 3), not here.
		vics := eligibleVictims(needNode, prioLimit, c.cfg.VictimsPerLevel)
		dests := topDestsByFree(c.nodes, c.cfg.DestsPerLevel)

		for _, y := range vics {
			if y.CPUm == 0 && y.MemBytes == 0 {
				continue
			}
			// quick win: move y off needNode to any other node that can accept it
			for _, dn := range dests {
				if dn.Name != needNode.Name && dn.Name != t.Name {
					if fitsWithReservations(dn, y, par, cur, c.allPods) {
						par[bfsKey{needNode: "OK", needUID: ""}] = parentEdge{
							prev: cur, mov: move{UID: y.UID, From: needNode.Name, To: dn.Name}, ok: true,
						}
						return reconstruct(par, bfsKey{needNode: "OK", needUID: ""}), true
					}
				}
			}
			// expand frontier: moving y into dn will require freeing dn
			for _, dn := range dests {
				if dn.Name == needNode.Name || dn.Name == t.Name {
					continue
				}
				next := bfsKey{needNode: dn.Name, needUID: y.UID}
				if !seen[next] && fitsWithReservations(dn, y, par, cur, c.allPods) {
					seen[next] = true
					par[next] = parentEdge{prev: cur, mov: move{UID: y.UID, From: needNode.Name, To: dn.Name}, ok: true}
					q = append(q, next)
				}
			}
		}
	}
	klog.V(V2).InfoS("BFS queue drained without success")
	return nil, false
}

func reconstruct(par map[bfsKey]parentEdge, terminal bfsKey) []move {
	out := make([]move, 0, 8)
	cur := terminal
	for {
		pe, ok := par[cur]
		if !ok || !pe.ok {
			break
		}
		out = append(out, pe.mov) // last→first for execution order
		cur = pe.prev
	}
	return out
}

/* =========================== Helpers & scoring =========================== */

type delta struct{ cpu, mem int64 }

// sum of deltas along the parent chain from 'cur' back to the root (last→first order)
func pathNetDeltas(par map[bfsKey]parentEdge, cur bfsKey, pods map[string]*pState) map[string]delta {
	acc := map[string]delta{}
	for {
		pe, ok := par[cur]
		if !ok || !pe.ok {
			break
		}
		mv := pe.mov
		p := pods[mv.UID]
		if p != nil {
			// leaving 'From' frees (+)
			df := acc[mv.From]
			df.cpu += p.CPUm
			df.mem += p.MemBytes
			acc[mv.From] = df
			// entering 'To' consumes (−)
			dt := acc[mv.To]
			dt.cpu -= p.CPUm
			dt.mem -= p.MemBytes
			acc[mv.To] = dt
		}
		cur = pe.prev
	}
	return acc
}

// check if node 'n' can accept pod 'y' given *current* path reservations
func fitsWithReservations(n *nState, y *pState, par map[bfsKey]parentEdge, cur bfsKey, pods map[string]*pState) bool {
	d := pathNetDeltas(par, cur, pods)[n.Name]
	// final two-phase free on this node if we add 'y' next:
	finalCPU := n.FreeCPUm + d.cpu - y.CPUm
	finalMem := n.FreeMem + d.mem - y.MemBytes
	return finalCPU >= 0 && finalMem >= 0
}

func dec(budget *int) bool {
	if budget == nil { // nil -> unlimited
		return true
	}
	if *budget <= 0 { // exhausted
		return false
	}
	*budget--
	return true
}

// Best direct-fit by CPU waste (tie: Mem waste).
func bestDirectFit(order []*nState, byName map[string]*nState, p *pState) (string, bool) {
	best := ""
	bestWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.fits(p.CPUm, p.MemBytes) {
			w := n.FreeCPUm - p.CPUm
			if w < bestWaste {
				bestWaste, best = w, n.Name
			} else if w == bestWaste {
				memBest := int64(math.MaxInt64)
				if best != "" {
					memBest = byName[best].FreeMem - p.MemBytes
				}
				if n.FreeMem-p.MemBytes < memBest {
					best = n.Name
				}
			}
		}
	}
	return best, best != ""
}

// Targets: smaller deficit first (CPU, then Mem).
func topKTargetsByDeficit(order []*nState, p *pState, K int) []*nState {
	ts := make([]*nState, len(order))
	copy(ts, order)
	sort.Slice(ts, func(i, j int) bool {
		diCPU := max64(0, p.CPUm-ts[i].FreeCPUm)
		djCPU := max64(0, p.CPUm-ts[j].FreeCPUm)
		if diCPU != djCPU {
			return diCPU < djCPU
		}
		diMem := max64(0, p.MemBytes-ts[i].FreeMem)
		djMem := max64(0, p.MemBytes-ts[j].FreeMem)
		return diMem < djMem
	})
	return ts[:capK(K, len(ts))]
}

func totalFree(order []*nState) (cpu, mem int64) {
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMem
	}
	return
}

// Coverage-biased victims on a node t (<= prioLimit), ordered by:
// (coverage score desc) → (lower priority) → (smaller).
func pickVictims(t *nState, prioLimit int, needCPU, needMem int64, K int) []*pState {
	type vic struct {
		p     *pState
		score int64
	}
	buf := make([]vic, 0, len(t.Pods))
	for _, rp := range t.Pods {
		if rp.Protected || int(rp.Priority) > prioLimit {
			continue
		}
		cg, mg := min64(rp.CPUm, needCPU), min64(rp.MemBytes, needMem)
		buf = append(buf, vic{p: rp, score: cg*3 + mg*2})
	}
	if len(buf) == 0 {
		return nil
	}
	sort.Slice(buf, func(i, j int) bool {
		if buf[i].score != buf[j].score {
			return buf[i].score > buf[j].score
		}
		if buf[i].p.Priority != buf[j].p.Priority {
			return buf[i].p.Priority < buf[j].p.Priority
		}
		if buf[i].p.CPUm != buf[j].p.CPUm {
			return buf[i].p.CPUm < buf[j].p.CPUm
		}
		return buf[i].p.MemBytes < buf[j].p.MemBytes
	})
	buf = buf[:capK(K, len(buf))]
	out := make([]*pState, len(buf))
	for i := range buf {
		out[i] = buf[i].p
	}
	return out
}

// Eligible victims: lower prio first, then smaller pods (cap by K).
func eligibleVictims(n *nState, prioLimit, K int) []*pState {
	buf := make([]*pState, 0, len(n.Pods))
	for _, rp := range n.Pods {
		if rp.Protected || int(rp.Priority) > prioLimit {
			continue
		}
		buf = append(buf, rp)
	}
	sort.Slice(buf, func(i, j int) bool {
		if buf[i].Priority != buf[j].Priority {
			return buf[i].Priority < buf[j].Priority
		}
		if buf[i].CPUm != buf[j].CPUm {
			return buf[i].CPUm < buf[j].CPUm
		}
		return buf[i].MemBytes < buf[j].MemBytes
	})
	return buf[:capK(K, len(buf))]
}

// Top destinations by free (CPU desc, then Mem desc).
func topDestsByFree(nodes map[string]*nState, K int) []*nState {
	ns := make([]*nState, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].FreeCPUm != ns[j].FreeCPUm {
			return ns[i].FreeCPUm > ns[j].FreeCPUm
		}
		return ns[i].FreeMem > ns[j].FreeMem
	})
	return ns[:capK(K, len(ns))]
}

// Best direct destination for pod p (min CPU waste, tie Mem).
func bestDirectDest(nodes map[string]*nState, p *pState) string {
	best := ""
	bestCPU := int64(math.MaxInt64)
	bestMEM := int64(math.MaxInt64)
	for _, n := range nodes {
		if n.Pods[p.UID] != nil { // same node
			continue
		}
		if n.fits(p.CPUm, p.MemBytes) {
			cw, mw := n.FreeCPUm-p.CPUm, n.FreeMem-p.MemBytes
			if cw < bestCPU || (cw == bestCPU && mw < bestMEM) {
				bestCPU, bestMEM, best = cw, mw, n.Name
			}
		}
	}
	return best
}

// Two-phase apply: remove all, then add all. Reject if final would overfill.
func applyTwoPhase(c *ctxFast, nodes map[string]*nState, pods map[string]*pState, mvs []move) bool {
	if len(mvs) == 0 {
		return true
	}
	type delta struct{ cpu, mem int64 }
	per := map[string]delta{}
	for _, mv := range mvs {
		p := pods[mv.UID]
		from, to := nodes[mv.From], nodes[mv.To]
		if p == nil || from == nil || to == nil {
			continue
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
	for name, d := range per {
		if n := nodes[name]; n != nil {
			finalCPU := n.FreeCPUm + d.cpu
			finalMem := n.FreeMem + d.mem
			if finalCPU < 0 || finalMem < 0 {
				klog.V(V2).InfoS("Rejecting BFS chain: final-state overfill",
					"node", name,
					"freeCPU_m_now", n.FreeCPUm, "freeMem_MiB_now", bytesToMiB(n.FreeMem),
					"deltaCPU_m", d.cpu, "deltaMem_MiB", bytesToMiB(d.mem),
					"finalCPU_m", finalCPU, "finalMem_MiB", bytesToMiB(finalMem))
				// Dump the chain at V=3 for post-mortem
				for i, mv := range mvs {
					if p := pods[mv.UID]; p != nil {
						klog.V(V2).InfoS("  move",
							"idx", i, "pod", podName(c, mv.UID),
							"from", mv.From, "to", mv.To,
							"cpu_m", p.CPUm, "mem_MiB", bytesToMiB(p.MemBytes))
					}
				}
				return false
			}
		}
	}

	for _, mv := range mvs { // remove
		if p := pods[mv.UID]; p != nil {
			if from := nodes[mv.From]; from != nil && from.Pods[p.UID] != nil {
				from.remove(p)
			}
		}
	}
	for _, mv := range mvs { // add
		p := pods[mv.UID]
		to := nodes[mv.To]
		if p == nil || to == nil {
			continue
		}
		if !to.fits(p.CPUm, p.MemBytes) {
			klog.ErrorS(nil, "Chain does not fit on destination",
				"pod", podName(&ctxFast{uidInfo: c.uidInfo}, p.UID), "to", mv.To)
			return false
		}
		to.add(p)
	}
	return true
}

func recordMoves(mvs []move, placements map[string]string) {
	for _, mv := range mvs {
		placements[mv.UID] = mv.To
	}
}
