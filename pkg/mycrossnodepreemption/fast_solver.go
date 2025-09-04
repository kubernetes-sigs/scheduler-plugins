// solver_fast.go
// ----------------------------------------------------------------------------
// Non-exponential "augmenting-path" mover for cross-node preemption with a
// single linear iteration budget per pending pod.
//
// Strategy per pending pod (sorted by priority desc):
//   1) Direct fit: place on a node that already fits (best-waste).
//   2) If the cluster has enough total free, try up to K targets (smallest
//      deficit) and for each, run a BFS-based relocation that finds a chain of
//      moves (lower-priority, non-protected pods only) to free the target.
//      No evictions in this step.
//   3) If BFS fails (or aggregate free insufficient), evict exactly one
//      lowest-priority non-protected victim that enables an immediate fit.
//
// Bounded work:
//   • Branching caps: kTargetsToTry, kVictimsPerLevel, kDestsPerLevel
//   • Linear budget: kMaxIterationsPerPod, decremented in BFS/outer free loop
//
// Expects types elsewhere: SolverInput, SolverOutput, SolverNode, SolverPod,
// PodLite, Score.
// ----------------------------------------------------------------------------

package mycrossnodepreemption

import (
	"math"
	"sort"

	"k8s.io/klog/v2"
)

/* ============================= Tunable knobs ============================== */

const (
	// Set to -1 for "no limit" or >0 for an explicit cap.
	kTargetsToTry        = -1    // Try top-K target nodes (smallest deficit)
	kVictimsPerLevel     = -1    // Consider up to K victims on a node
	kDestsPerLevel       = -1    // Consider up to K destination nodes
	kMaxIterationsPerPod = 10000 // Linear budget per pending pod (shared across BFS/relocations)
)

/* =========================== Internal light state ========================= */

type podState struct {
	UID       string
	CPUm      int64
	MemBytes  int64
	Priority  int32
	Protected bool
	Node      string // current placement ("" means pending)
}

type nodeState struct {
	Name     string
	CapCPUm  int64
	CapMem   int64
	FreeCPUm int64
	FreeMem  int64
	Pods     map[string]*podState

	// debug
	StartFreeCPUm int64 // after loading existing pods, before placing any pending ones
	StartFreeMem  int64
}

// limitCount interprets K with special values: -1 = unlimited, 0 = zero, >0 = min(K, n).
func limitCount(K, n int) int {
	if K < 0 { // unlimited
		return n
	}
	if K == 0 {
		return 0
	}
	if K > n {
		return n
	}
	return K
}

// budgetDec decrements the iteration budget if finite.
// Returns false when the budget is exhausted and work must stop.
func budgetDec(budget *int) bool {
	if budget == nil {
		return true
	}
	if *budget < 0 { // unlimited
		return true
	}
	if *budget == 0 {
		return false
	}
	*budget--
	return true
}

func (n *nodeState) fits(cpu, mem int64) bool { return n.FreeCPUm >= cpu && n.FreeMem >= mem }
func (n *nodeState) addPod(p *podState) {
	n.FreeCPUm -= p.CPUm
	n.FreeMem -= p.MemBytes
	n.Pods[p.UID] = p
	p.Node = n.Name
}
func (n *nodeState) removePod(p *podState) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.FreeCPUm += p.CPUm
		n.FreeMem += p.MemBytes
		p.Node = ""
	}
}

type moveAction struct{ UID, From, To string }

/* =============================== Solver entry ============================= */

func runFastSolver(in SolverInput) *SolverOutput {

	// Materialize nodes
	nodes := make(map[string]*nodeState, len(in.Nodes))
	nodeList := make([]*nodeState, 0, len(in.Nodes))
	for i := range in.Nodes {
		n := &nodeState{
			Name:     in.Nodes[i].Name,
			CapCPUm:  in.Nodes[i].CPUm,
			CapMem:   in.Nodes[i].MemBytes,
			FreeCPUm: in.Nodes[i].CPUm,
			FreeMem:  in.Nodes[i].MemBytes,
			Pods:     make(map[string]*podState, 64),
		}
		nodes[n.Name] = n
		nodeList = append(nodeList, n)
	}

	// Materialize pods
	allPods := make(map[string]*podState, len(in.Pods)+1)
	var pendingList []*podState
	// Keep UID → (ns,name,uid) so we can build NewPlacements later.
	uidInfo := make(map[string]PodLite, len(in.Pods)+1)

	if in.Preemptor != nil {
		pre := &podState{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
			Node:      "",
		}
		allPods[pre.UID] = pre
		pendingList = append(pendingList, pre)
		uidInfo[pre.UID] = PodLite{
			UID:       in.Preemptor.UID,
			Namespace: in.Preemptor.Namespace,
			Name:      in.Preemptor.Name,
		}
	}

	for i := range in.Pods {
		sp := in.Pods[i]
		p := &podState{
			UID:       sp.UID,
			CPUm:      sp.CPU_m,
			MemBytes:  sp.MemBytes,
			Priority:  sp.Priority,
			Protected: sp.Protected,
			Node:      sp.Where,
		}
		allPods[p.UID] = p
		uidInfo[p.UID] = PodLite{UID: sp.UID, Namespace: sp.Namespace, Name: sp.Name}
		if p.Node == "" {
			pendingList = append(pendingList, p)
		} else if n := nodes[p.Node]; n != nil {
			n.addPod(p)
		}
	}
	for _, n := range nodeList {
		n.StartFreeCPUm = n.FreeCPUm
		n.StartFreeMem = n.FreeMem
	}

	// Highest priority first; then larger CPU; then Mem
	sort.Slice(pendingList, func(i, j int) bool {
		if pendingList[i].Priority != pendingList[j].Priority {
			return pendingList[i].Priority > pendingList[j].Priority
		}
		if pendingList[i].CPUm != pendingList[j].CPUm {
			return pendingList[i].CPUm > pendingList[j].CPUm
		}
		return pendingList[i].MemBytes > pendingList[j].MemBytes
	})

	// Build final placements as last-destination-per-UID, then convert to []NewPlacements.
	var placementsByUID = make(map[string]string) // podUID → nodeName
	var evictedUIDs []PodLite

	// Main loop over pending pods
	preUID := ""
	if in.Preemptor != nil {
		preUID = in.Preemptor.UID
	}
	for _, p := range pendingList {
		_ = placeOneBFS(nodes, nodeList, allPods, p, placementsByUID, &evictedUIDs, preUID)
		if in.Preemptor != nil && p.UID == in.Preemptor.UID {
			break
		}
	}

	status := "FEASIBLE"
	if in.Preemptor != nil {
		if _, ok := placementsByUID[in.Preemptor.UID]; !ok {
			status = "INFEASIBLE"
		}
	}

	// Convert map → stable, deduped []NewPlacements (sorted by UID for determinism).
	uidList := make([]string, 0, len(placementsByUID))
	for uid := range placementsByUID {
		uidList = append(uidList, uid)
	}
	sort.Strings(uidList)
	placementsList := make([]NewPlacements, 0, len(uidList))
	for _, uid := range uidList {
		dest := placementsByUID[uid]
		if dest == "" {
			continue
		}
		info := uidInfo[uid]
		placementsList = append(placementsList, NewPlacements{
			Pod:        PodLite{UID: info.UID, Namespace: info.Namespace, Name: info.Name},
			TargetNode: dest,
		})
	}

	if in.Preemptor != nil {
		if dest, ok := placementsByUID[in.Preemptor.UID]; ok {
			if n := nodes[dest]; n != nil {
				// after-placing snapshot
				afterCPU := n.FreeCPUm
				afterMem := n.FreeMem
				// what we believed immediately before placing preemptor on dest
				beforeCPU := afterCPU + in.Preemptor.CPU_m
				beforeMem := afterMem + in.Preemptor.MemBytes

				klog.InfoS("Nominate node for preemptor",
					"uid", in.Preemptor.UID,
					"node", dest,
					"podCPU_m", in.Preemptor.CPU_m,
					"podMem_MiB", bytesToMiB(in.Preemptor.MemBytes),
					"startFreeCPU_m", n.StartFreeCPUm,
					"startFreeMem_MiB", bytesToMiB(n.StartFreeMem),
					"freeCPU_m_before", beforeCPU,
					"freeMem_MiB_before", bytesToMiB(beforeMem),
					"freeCPU_m_after", afterCPU,
					"freeMem_MiB_after", bytesToMiB(afterMem),
					"capCPU_m", n.CapCPUm,
					"capMem_MiB", bytesToMiB(n.CapMem),
					"movesInPlanSoFar", len(placementsByUID)-1, // rough indicator
				)

				// extra safety: scream if we somehow nominated without fit
				if beforeCPU < in.Preemptor.CPU_m || beforeMem < in.Preemptor.MemBytes {
					klog.ErrorS(nil, "BUG: nominated node without enough free at placement time",
						"uid", in.Preemptor.UID, "node", dest,
						"needCPU_m", in.Preemptor.CPU_m, "needMem_MiB", bytesToMiB(in.Preemptor.MemBytes),
						"freeCPU_m_before", beforeCPU, "freeMem_MiB_before", bytesToMiB(beforeMem))
				}
			}
		}
	}
	return &SolverOutput{
		Status:     status,
		Placements: placementsList,
		Evictions:  evictedUIDs,
	}
}

func logPrePost(p *podState, n *nodeState, via string, beforeCPU, beforeMem int64) {
	klog.InfoS("Preemptor placement",
		"via", via,
		"uid", p.UID,
		"priority", p.Priority,
		"node", n.Name,
		"podCPU_m", p.CPUm,
		"podMem_MiB", bytesToMiB(p.MemBytes),
		"startFreeCPU_m", n.StartFreeCPUm,
		"startFreeMem_MiB", bytesToMiB(n.StartFreeMem),
		"freeCPU_m_before", beforeCPU,
		"freeMem_MiB_before", bytesToMiB(beforeMem),
		"freeCPU_m_after", n.FreeCPUm,
		"freeMem_MiB_after", bytesToMiB(n.FreeMem),
		"capCPU_m", n.CapCPUm,
		"capMem_MiB", bytesToMiB(n.CapMem),
	)
}

func guardMustFit(where string, n *nodeState, p *podState, beforeCPU, beforeMem int64) {
	if beforeCPU < p.CPUm || beforeMem < p.MemBytes {
		klog.ErrorS(nil, "BUG: placing on node that does not fit",
			"path", where,
			"uid", p.UID, "node", n.Name,
			"needCPU_m", p.CPUm, "needMem_MiB", bytesToMiB(p.MemBytes),
			"freeCPU_m_before", beforeCPU, "freeMem_MiB_before", bytesToMiB(beforeMem),
			"freeCPU_m_after", n.FreeCPUm, "freeMem_MiB_after", bytesToMiB(n.FreeMem))
	}
}

/* ======================== Placement for a single pod ======================= */

// placeOneBFS: direct-fit → BFS relocation (no evictions) → single-eviction fallback.
// Uses a shared linear iteration budget per pending pod.
func placeOneBFS(
	nodes map[string]*nodeState,
	nodeList []*nodeState,
	allPods map[string]*podState,
	p *podState,
	placementsByUID map[string]string,
	evictedUIDs *[]PodLite,
	preUID string, // "" if none
) bool {
	// Linear budget shared across freeing iterations and BFS expansions.
	budget := kMaxIterationsPerPod

	// 1) Direct fit
	if nodeName, ok := selectDirectFit(nodes, nodeList, p); ok {
		n := nodes[nodeName]
		beforeCPU, beforeMem := n.FreeCPUm, n.FreeMem
		guardMustFit("direct_fit", n, p, beforeCPU, beforeMem)
		n.addPod(p)
		placementsByUID[p.UID] = nodeName
		if p.UID == preUID {
			logPrePost(p, n, "direct_fit", beforeCPU, beforeMem)
		}
		return true
	}

	// 2) BFS relocations if aggregate free is sufficient
	totalCPU, totalMem := totalFreeResources(nodeList)
	if totalCPU >= p.CPUm && totalMem >= p.MemBytes {
		targets := topKTargetsByDeficit(nodeList, p, kTargetsToTry)
		for _, t := range targets {
			if t.fits(p.CPUm, p.MemBytes) {
				t.addPod(p)
				placementsByUID[p.UID] = t.Name
				return true
			}
			moves, ok := bfsFreeTargetFor(nodes, allPods, t, p, int(p.Priority), &budget)
			if ok {
				// BFS already applied the moves to state; just record them so they reach the plan
				recordMovesOnly(moves, placementsByUID)
				// Safety: the BFS chain should have freed enough on t now.
				beforeCPU, beforeMem := t.FreeCPUm, t.FreeMem
				guardMustFit("bfs_chain", t, p, beforeCPU, beforeMem)
				t.addPod(p)
				placementsByUID[p.UID] = t.Name
				if p.UID == preUID {
					logPrePost(p, t, "bfs_chain", beforeCPU, beforeMem)
				}
				return true
			}
			// If budget exhausted, stop trying more targets for this pod.
			if budget <= 0 {
				break
			}
		}
	}

	// 3) Single-eviction fallback
	victim, vNode := pickSingleEvictionCandidate(nodeList, p)
	if victim == nil || vNode == nil {
		return false
	}
	vNode.removePod(victim)
	*evictedUIDs = append(*evictedUIDs, PodLite{UID: victim.UID})

	beforeCPU, beforeMem := vNode.FreeCPUm, vNode.FreeMem
	guardMustFit("single_eviction", vNode, p, beforeCPU, beforeMem)
	vNode.addPod(p)
	placementsByUID[p.UID] = vNode.Name
	if p.UID == preUID {
		logPrePost(p, vNode, "single_eviction", beforeCPU, beforeMem)
	}
	return true
}

func recordMovesOnly(moves []moveAction, placementsByUID map[string]string) {
	for _, mv := range moves {
		placementsByUID[mv.UID] = mv.To
	}
}

/* =============================== BFS building ============================= */

// bfsFreeTargetFor frees target node 't' enough to fit pod 'p' by iteratively
// relocating exactly one victim off 't' using BFS chains (no evictions).
// Only moves pods with Priority < p.Priority and not Protected.
// The linear *budget is decremented once per "free-one-victim" attempt.
// Returns the concatenated move list (in order) or false if it gives up.
func bfsFreeTargetFor(
	nodes map[string]*nodeState,
	allPods map[string]*podState,
	t *nodeState,
	p *podState,
	prioLimit int,
	budget *int,
) ([]moveAction, bool) {
	totalMoves := make([]moveAction, 0, 16)
	for !t.fits(p.CPUm, p.MemBytes) {
		if !budgetDec(budget) {
			return nil, false
		}

		needCPU := max64(0, p.CPUm-t.FreeCPUm)
		needMem := max64(0, p.MemBytes-t.FreeMem)

		victims := pickVictimsKHop(t, prioLimit, needCPU, needMem, kVictimsPerLevel)
		if len(victims) == 0 {
			return nil, false
		}

		var chain []moveAction
		ok := false

		for _, v0 := range victims {
			if dest := bestDirectDest(nodes, v0); dest != "" && dest != t.Name {
				chain = []moveAction{{UID: v0.UID, From: t.Name, To: dest}}
				ok = true
				break
			}
			if c, ok2 := bfsRelocateOne(nodes, t, v0, prioLimit, budget); ok2 {
				chain = c
				ok = true
				break
			}
			// no explicit budget <= 0 check here—bfsRelocateOne already consumes it
		}

		if !ok {
			return nil, false
		}

		applyMovesBare(nodes, allPods, chain)
		totalMoves = append(totalMoves, chain...)
	}
	return totalMoves, true
}

/* =============================== BFS internals ============================ */

// BFS state: “we need node `needNode` to be able to host pod `needUID`”.
type bfsKey struct {
	needNode string
	needUID  string
}

type bfsParent struct {
	prev bfsKey
	move moveAction // move that frees 'prev.needNode' to host 'prev.needUID'
	ok   bool
}

// Relocate exactly one victim v0 out of target t via an augmenting-path BFS.
// Decrements *budget once per dequeued state (linear bound).
func bfsRelocateOne(
	nodes map[string]*nodeState,
	t *nodeState,
	v0 *podState,
	prioLimit int,
	budget *int,
) ([]moveAction, bool) {
	start := bfsKey{needNode: t.Name, needUID: v0.UID}
	q := []bfsKey{start}
	parent := map[bfsKey]bfsParent{}
	seen := map[bfsKey]bool{start: true}

	for len(q) > 0 {
		if !budgetDec(budget) {
			return nil, false
		}

		cur := q[0]
		q = q[1:]
		needNode := nodes[cur.needNode]
		if needNode == nil {
			continue
		}

		vicList := eligibleVictimsOn(needNode, prioLimit, kVictimsPerLevel)
		dests := topKDestsByFree(nodes, kDestsPerLevel)

		for _, y := range vicList {
			for _, dn := range dests {
				if dn.Name == needNode.Name {
					continue
				}
				if dn.fits(y.CPUm, y.MemBytes) {
					parent[bfsKey{needNode: "SUCCESS", needUID: ""}] =
						bfsParent{prev: cur, move: moveAction{UID: y.UID, From: needNode.Name, To: dn.Name}, ok: true}
					chain := reconstructBFSChain(parent, bfsKey{needNode: "SUCCESS", needUID: ""})
					return chain, true
				}
			}
			for _, dn := range dests {
				if dn.Name == needNode.Name {
					continue
				}
				next := bfsKey{needNode: dn.Name, needUID: y.UID}
				if seen[next] {
					continue
				}
				seen[next] = true
				parent[next] = bfsParent{
					prev: cur,
					move: moveAction{UID: y.UID, From: needNode.Name, To: dn.Name},
					ok:   true,
				}
				q = append(q, next)
			}
		}
	}
	return nil, false
}

// Reconstruct BFS path by following parent links from the terminal marker.
// IMPORTANT: return moves in *execution* order = last -> first.
// Executing last first ensures every move fits without temporary overfill.
func reconstructBFSChain(parent map[bfsKey]bfsParent, terminal bfsKey) []moveAction {
	chain := make([]moveAction, 0, 8)
	cur := terminal
	for {
		par, ok := parent[cur]
		if !ok || !par.ok {
			break
		}
		chain = append(chain, par.move) // append last→first
		cur = par.prev
	}
	return chain
}

/* =========================== Helpers & scoring ============================ */

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

// Pick best direct-fit node (minimize CPU waste, tie-break by Mem waste).
func selectDirectFit(nodes map[string]*nodeState, order []*nodeState, p *podState) (string, bool) {
	bestNode := ""
	bestWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.fits(p.CPUm, p.MemBytes) {
			waste := n.FreeCPUm - p.CPUm
			if waste < bestWaste {
				bestWaste = waste
				bestNode = n.Name
			} else if waste == bestWaste {
				memWasteBest := int64(math.MaxInt64)
				if bestNode != "" {
					memWasteBest = nodes[bestNode].FreeMem - p.MemBytes
				}
				memWaste := n.FreeMem - p.MemBytes
				if memWaste < memWasteBest {
					bestNode = n.Name
				}
			}
		}
	}
	return bestNode, bestNode != ""
}

// Order nodes by deficit for p (CPU first, then Mem). Smaller deficit first.
func orderTargetsByDeficit(order []*nodeState, p *podState) []*nodeState {
	targets := make([]*nodeState, len(order))
	copy(targets, order)
	sort.Slice(targets, func(i, j int) bool {
		diCPU := max64(0, p.CPUm-targets[i].FreeCPUm)
		djCPU := max64(0, p.CPUm-targets[j].FreeCPUm)
		if diCPU != djCPU {
			return diCPU < djCPU
		}
		diMem := max64(0, p.MemBytes-targets[i].FreeMem)
		djMem := max64(0, p.MemBytes-targets[j].FreeMem)
		return diMem < djMem
	})
	return targets
}

func topKTargetsByDeficit(order []*nodeState, p *podState, K int) []*nodeState {
	ts := orderTargetsByDeficit(order, p)
	take := limitCount(K, len(ts))
	return ts[:take]
}

func totalFreeResources(order []*nodeState) (int64, int64) {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMem
	}
	return cpu, mem
}

// Coverage-biased victims on node t (strictly lower prio than pending pod),
// ordered by (coverage score desc) → (lower prio) → (smaller).
func pickVictimsKHop(t *nodeState, prioLimit int, needCPU, needMem int64, K int) []*podState {
	type vic struct {
		p     *podState
		score int64
	}
	buf := make([]vic, 0, len(t.Pods))
	for _, rp := range t.Pods {
		if rp.Protected || int(rp.Priority) >= prioLimit {
			continue
		}
		cg := min64(rp.CPUm, needCPU)
		mg := min64(rp.MemBytes, needMem)
		sc := cg*3 + mg*2 // heuristic: emphasize CPU slightly
		buf = append(buf, vic{p: rp, score: sc})
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
	buf = buf[:limitCount(K, len(buf))]
	out := make([]*podState, len(buf))
	for i := range buf {
		out[i] = buf[i].p
	}
	return out
}

// Apply moves to real state (no placements bookkeeping).
func applyMovesBare(nodes map[string]*nodeState, pods map[string]*podState, moves []moveAction) {
	for _, mv := range moves {
		p := pods[mv.UID]
		if p == nil {
			continue
		}
		from := nodes[mv.From]
		to := nodes[mv.To]
		if from == nil || to == nil {
			continue
		}
		if from.Pods[mv.UID] == nil {
			continue
		}
		from.removePod(p)
		// Defensive check: with correct chain order this should always fit.
		if !to.fits(p.CPUm, p.MemBytes) {
			klog.ErrorS(nil, "BUG: applying move that does not fit (check chain order)",
				"podUID", p.UID, "from", from.Name, "to", to.Name,
				"needCPU_m", p.CPUm, "needMem_MiB", bytesToMiB(p.MemBytes),
				"freeCPU_m", to.FreeCPUm, "freeMem_MiB", bytesToMiB(to.FreeMem))
		}
		to.addPod(p)
	}
}

// Single-eviction candidate: pick one victim that enables immediate fit (globally cheapest).
func pickSingleEvictionCandidate(order []*nodeState, p *podState) (*podState, *nodeState) {
	var bestVictim *podState
	var bestNode *nodeState
	var bestKey struct {
		priority int32
		cpu, mem int64
	}
	bestKey.priority = math.MaxInt32
	bestKey.cpu = math.MaxInt64
	bestKey.mem = math.MaxInt64

	for _, n := range order {
		needCPU := p.CPUm - n.FreeCPUm
		needMem := p.MemBytes - n.FreeMem
		if needCPU <= 0 && needMem <= 0 {
			continue // already fits
		}
		cands := make([]*podState, 0, len(n.Pods))
		for _, rp := range n.Pods {
			if rp.Protected || rp.Priority >= p.Priority {
				continue
			}
			cands = append(cands, rp)
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].Priority != cands[j].Priority {
				return cands[i].Priority < cands[j].Priority
			}
			if cands[i].CPUm != cands[j].CPUm {
				return cands[i].CPUm < cands[j].CPUm
			}
			return cands[i].MemBytes < cands[j].MemBytes
		})
		for _, v := range cands {
			if n.FreeCPUm+v.CPUm >= p.CPUm && n.FreeMem+v.MemBytes >= p.MemBytes {
				key := struct {
					priority int32
					cpu, mem int64
				}{v.Priority, v.CPUm, v.MemBytes}
				if key.priority < bestKey.priority ||
					(key.priority == bestKey.priority && (key.cpu < bestKey.cpu ||
						(key.cpu == bestKey.cpu && key.mem < bestKey.mem))) {
					bestVictim = v
					bestNode = n
					bestKey = key
				}
				break // best on this node
			}
		}
	}
	return bestVictim, bestNode
}

/* ====================== Small utilities used by BFS ======================= */

// Eligible victims on a node, ordered: lower prio first, then smaller pods.
// limit: -1 unlimited, 0 none, >0 cap
func eligibleVictimsOn(n *nodeState, prioLimit int, limit int) []*podState {
	buf := make([]*podState, 0, len(n.Pods))
	for _, rp := range n.Pods {
		if rp.Protected || int(rp.Priority) >= prioLimit {
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
	take := limitCount(limit, len(buf))
	return buf[:take]
}

// Top-K destinations by current free (CPU, then Mem) in descending order.
func topKDestsByFree(nodes map[string]*nodeState, K int) []*nodeState {
	ns := make([]*nodeState, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].FreeCPUm != ns[j].FreeCPUm {
			return ns[i].FreeCPUm > ns[j].FreeCPUm
		}
		return ns[i].FreeMem > ns[j].FreeMem
	})
	take := limitCount(K, len(ns))
	return ns[:take]
}

// Best direct destination for a given pod (min CPU waste, tie by Mem).
func bestDirectDest(nodes map[string]*nodeState, p *podState) string {
	best := ""
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range nodes {
		if n.Pods[p.UID] != nil { // skip current node
			continue
		}
		if n.fits(p.CPUm, p.MemBytes) {
			cw := n.FreeCPUm - p.CPUm
			mw := n.FreeMem - p.MemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && mw < bestMEMWaste) {
				bestCPUWaste, bestMEMWaste = cw, mw
				best = n.Name
			}
		}
	}
	return best
}
