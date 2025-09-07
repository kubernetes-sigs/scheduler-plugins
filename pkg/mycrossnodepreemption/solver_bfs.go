package mycrossnodepreemption

import (
	"fmt"
	"math"
	"runtime"
	"sort"

	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Unified entry point: single preemptor or batch (mirrors runSolverSwap)
// -----------------------------------------------------------------------------

func runSolverBfs(in SolverInput) *SolverOutput {
	nodes, pods, pending, order, pre := buildClusterState(in)

	if len(pending) == 0 {
		klog.InfoS("bfs: nothing to place")
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	// A. Worklist (big-first), same as swap
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

		placed, infeasible := placeOnePodBfs(
			p, nodes, pods, order,
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
			klog.InfoS("bfs: stopping batch at priority; discarding remainder",
				"priority", stopAt, "uid", p.UID)
		}
	}

	return stableOutput("FEASIBLE", newPlacements, evicts, in)
}

// -----------------------------------------------------------------------------
// Per-pod BFS pipeline (same shape as placeOnePod, but step 3 uses BFS)
// -----------------------------------------------------------------------------

func placeOnePodBfs(
	p *pLite,
	nodes map[string]*nLite,
	pods map[string]*pLite,
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

	// 3) Moves-only via BFS (fewest moves on the best target)
	if placeByBFS(p, nodes, pods, order, moveGate, movedUIDs, newPlacements) {
		return true, false
	}

	// 4) Evict (strictly lower prio & enabling-only)
tryEvict:
	v, on := pickLargestEnablingEviction(order, p, evictGate, movedUIDs)
	if v == nil || on == nil {
		return false, true
	}
	delete(newPlacements, v.UID)
	delete(movedUIDs, v.UID)
	on.remove(v)
	*evicts = append(*evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})

	if on.fits(p.CPUm, p.MemBytes) {
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
// Moves-only using BFS: finish each target up to MaxDepth, pick fewest moves
// -----------------------------------------------------------------------------

func placeByBFS(
	p *pLite,
	nodes map[string]*nLite,
	pods map[string]*pLite,
	order []*nLite,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) bool {
	targets := orderTargetsByDeficit(order, p)

	// build orig map once for squashMoves
	orig := make(map[string]string, 256)
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Node != "" {
				orig[q.UID] = q.Node
			}
		}
	}

	var bestMoves []moveLite
	bestTarget := ""
	bestCount := math.MaxInt32
	found := false

	// Moves gate policy:
	//  - single-preemptor: gate = pre.Priority (passed via moveGate)
	//  - batch mode: gate disabled -> allow all priorities (respect Protected)
	prioLimit := int32(math.MaxInt32)
	if moveGate != nil {
		prioLimit = *moveGate
	}

	for _, t := range targets {
		needCPU := max64(0, p.CPUm-t.FreeCPUm)
		needMem := max64(0, p.MemBytes-t.FreeMemBytes)

		if needCPU == 0 && needMem == 0 {
			bestTarget = t.Name
			bestMoves = nil
			bestCount = 0
			found = true
			break
		}

		// Run your layered BFS for this target (fewest moves for this target).
		// Note: bfsFreeTargets already respects priority via prioLimit and Protected via your filters.
		fd, ok := bfsFreeTargets(nodes, t.Name, needCPU, needMem, prioLimit)
		if !ok {
			continue
		}

		coalesced := squashMoves(fd, orig)
		if len(coalesced) < bestCount {
			bestMoves = coalesced
			bestTarget = t.Name
			bestCount = len(coalesced)
			found = true
			if bestCount <= 1 { // can't beat 0/1
				break
			}
		}
	}

	if !found {
		return false
	}

	// Commit chosen plan and place p
	if !verifyCoalescedPlan(nodes, pods, bestMoves, p, bestTarget) {
		return false
	}
	if !applyTwoPhase(nodes, pods, bestMoves) {
		return false
	}
	for _, mv := range bestMoves {
		newPlacements[mv.UID] = mv.To
		movedUIDs[mv.UID] = struct{}{}
	}
	if n := nodes[bestTarget]; n != nil && n.fits(p.CPUm, p.MemBytes) {
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

func (n *nLite) fits(cpu, mem int64) bool { return n.FreeCPUm >= cpu && n.FreeMemBytes >= mem }
func (n *nLite) addPod(p *pLite) {
	n.FreeCPUm -= p.CPUm
	n.FreeMemBytes -= p.MemBytes
	n.Pods[p.UID] = p
	p.Node = n.Name
}
func (n *nLite) remove(p *pLite) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.FreeCPUm += p.CPUm
		n.FreeMemBytes += p.MemBytes
		p.Node = ""
	}
}

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
			fCPU := n.FreeCPUm + dd.cpu
			fMEM := n.FreeMemBytes + dd.mem
			if fCPU < 0 || fMEM < 0 {
				klog.InfoS("verify: final negative free", "node", name,
					"freeCPU_now", n.FreeCPUm, "freeMem_now", n.FreeMemBytes,
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
		if ns[i].FreeCPUm != ns[j].FreeCPUm {
			return ns[i].FreeCPUm > ns[j].FreeCPUm
		}
		if ns[i].FreeMemBytes != ns[j].FreeMemBytes {
			return ns[i].FreeMemBytes > ns[j].FreeMemBytes
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
			if n.FreeCPUm+dd.cpu < 0 || n.FreeMemBytes+dd.mem < 0 {
				klog.InfoS("apply: reject, final negative free", "node", name,
					"freeCPU_now", n.FreeCPUm, "freeMem_now", n.FreeMemBytes,
					"deltaCPU", dd.cpu, "deltaMem", dd.mem,
					"finalCPU", n.FreeCPUm+dd.cpu, "finalMem", n.FreeMemBytes+dd.mem)
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
					"uid", p.UID, "to", n.Name, "freeCPU_now", n.FreeCPUm, "freeMem_now", n.FreeMemBytes,
					"needCPU", p.CPUm, "needMem", p.MemBytes)
				return false
			}
			n.addPod(p)
		}
	}
	return true
}

// ============================ small utils ============================

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
	nodes map[string]*nLite,
	rootTarget string,
	rootNeedCPU, rootNeedMem int64,
	prioLimit int32,
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

	for depth := 0; depth <= SolverBfsMaxDepth; depth++ {
		// goal check within this layer
		for _, st := range front {
			if len(st.deficits) == 0 {
				goalDepth = depth
				foundRawSteps = len(st.moves)
				foundChainVictims = len(st.finalDest)
				return st.finalDest, true
			}
		}
		if depth == SolverBfsMaxDepth {
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
				SolverBfsMaxVictimsPerNode,
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
			dests := destsByFree(nodes, SolverBfsMaxDestsPerLevel)

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
					needCPU2 := max64(0, v.CPUm-(dn.FreeCPUm+dstRes.cpu))
					needMem2 := max64(0, v.MemBytes-(dn.FreeMemBytes+dstRes.mem))

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
