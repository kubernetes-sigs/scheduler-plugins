// solver_fast.go
// K-hop (bounded) move solver that prioritizes placing higher-priority pods with minimal evictions.
// For each pending pod (sorted by priority desc):
//   1) Try direct fit.
//   2) If the cluster has enough total free, try K-hop move chains on the best target nodes
//      (bounded depth, small branching; moves only lower-priority, non-protected pods; no evictions).
//   3) If K-hop fails (or aggregate free insufficient), evict exactly one lowest-priority non-protected
//      victim that enables an immediate fit, then retry (1).
//
// The algorithm is simple, bounded, and eviction-averse. It carries forward simulated state across
// pending pods so higher-priority pods get placed first, maximizing their count.
//
// Expects these types elsewhere in the package: SolverInput, SolverOutput, SolverNode, SolverPod,
// SolverEviction, Score.

package mycrossnodepreemption

import (
	"math"
	"sort"
	"strconv"
)

/* ----------------------------- Small, fixed knobs ----------------------------- */

const (
	kMaxHops         = 5 // maximum chain depth (number of moves) to free space
	kTargetsToTry    = 5 // try top-K target nodes (smallest deficit)
	kVictimsPerLevel = 6 // consider up to K victims per DFS frame
	kDestsPerLevel   = 6 // consider up to K destinations per DFS frame
)

/* ----------------------------- Internal state ----------------------------- */

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
	Pods     map[string]*podState // resident pods by UID
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

/* ----------------------------- Solver entry ----------------------------- */

func runFastSolver(in SolverInput) *SolverOutput {
	// Build nodes
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

	// Build pods
	allPods := make(map[string]*podState, len(in.Pods)+1)
	var pendingList []*podState

	// Include preemptor (if any) as pending
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
	}

	// Other pods (some placed, some pending)
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
		if p.Node == "" {
			pendingList = append(pendingList, p)
		} else if n := nodes[p.Node]; n != nil {
			n.addPod(p)
		}
	}

	// Sort pending: priority desc, then CPU desc, then Mem desc
	sort.Slice(pendingList, func(i, j int) bool {
		if pendingList[i].Priority != pendingList[j].Priority {
			return pendingList[i].Priority > pendingList[j].Priority
		}
		if pendingList[i].CPUm != pendingList[j].CPUm {
			return pendingList[i].CPUm > pendingList[j].CPUm
		}
		return pendingList[i].MemBytes > pendingList[j].MemBytes
	})

	placements := make(map[string]string, len(pendingList))
	var evictedUIDs []SolverEviction

	// Track original placements for move counting
	originalNode := make(map[string]string, len(allPods))
	for uid, p := range allPods {
		originalNode[uid] = p.Node
	}

	// Per pending pod
	for _, p := range pendingList {
		if placeOneKHop(nodes, nodeList, allPods, p, placements, &evictedUIDs) {
			// Single-preemptor flows end after placing or failing that one pod
			if in.Preemptor != nil && p.UID == in.Preemptor.UID {
				break
			}
		} else {
			if in.Preemptor != nil && p.UID == in.Preemptor.UID {
				break
			}
		}
	}

	// Score
	score := buildScoreFromState(allPods, originalNode, evictedUIDs)

	out := &SolverOutput{
		Status:        "FEASIBLE",
		NominatedNode: "",
		Placements:    placements,
		Evictions:     evictedUIDs,
		Score:         score,
	}

	// Nominate destination for single-preemptor
	if in.Preemptor != nil {
		if dest, ok := placements[in.Preemptor.UID]; ok {
			out.NominatedNode = dest
		}
	}
	return out
}

/* ----------------------------- K-hop placement ----------------------------- */

func placeOneKHop(
	nodes map[string]*nodeState,
	nodeList []*nodeState,
	allPods map[string]*podState,
	p *podState,
	placements map[string]string,
	evictedUIDs *[]SolverEviction,
) bool {
	// 1) Direct fit
	if nodeName, ok := selectDirectFit(nodes, nodeList, p); ok {
		nodes[nodeName].addPod(p)
		placements[p.UID] = nodeName
		return true
	}

	// 2) If aggregate free can hold p, try K-hop move chains (no evictions)
	totalCPU, totalMem := totalFreeResources(nodeList)
	if totalCPU >= p.CPUm && totalMem >= p.MemBytes {
		targets := topKTargetsByDeficit(nodeList, p, kTargetsToTry)
		if len(targets) > 0 {
			// Try targets in order; stop on first success
			for _, t := range targets {
				if t.fits(p.CPUm, p.MemBytes) {
					t.addPod(p)
					placements[p.UID] = t.Name
					return true
				}
				// Snapshot for backtracking
				snap := newSnapshot(nodes, allPods)
				moves := make([]moveAction, 0, kMaxHops)
				visited := make(map[string]bool, 16) // avoid moving the same UID multiple times

				if dfsKHopFree(snap, t.Name, p, int(p.Priority), kMaxHops, visited, &moves) {
					// Apply moves to real state and place
					applyMovesAndRecord(nodes, allPods, moves, placements)
					nodes[t.Name].addPod(p)
					placements[p.UID] = t.Name
					return true
				}
			}
		}
	}

	// 3) Single eviction (strict last resort). Evict one lowest-priority non-protected victim
	//    whose eviction allows immediate fit, then retry direct fit once.
	victim, vNode := pickSingleEvictionCandidate(nodeList, p)
	if victim == nil || vNode == nil {
		return false
	}
	delete(vNode.Pods, victim.UID)
	vNode.FreeCPUm += victim.CPUm
	vNode.FreeMem += victim.MemBytes
	*evictedUIDs = append(*evictedUIDs, SolverEviction{UID: victim.UID})

	// Retry direct fit after eviction (guaranteed to fit on vNode, but choose best anyway)
	if nodeName, ok := selectDirectFit(nodes, nodeList, p); ok {
		nodes[nodeName].addPod(p)
		placements[p.UID] = nodeName
		return true
	}
	return false
}

/* ----------------------------- K-hop DFS (simple) ----------------------------- */

// dfsKHopFree tries to free 'target' for pod p using <= depth moves.
// It may recursively free destinations. Only moves pods with Priority < p.Priority and not Protected.
func dfsKHopFree(
	snap *snapshot,
	target string,
	p *podState,
	prioLimit int,
	depth int,
	visited map[string]bool,
	plan *[]moveAction,
) bool {
	if depth < 0 {
		return false
	}
	if snap.nodes[target].fits(p.CPUm, p.MemBytes) {
		return true
	}
	if depth == 0 {
		return false
	}

	t := snap.nodes[target]
	needCPU := max64(0, p.CPUm-t.FreeCPUm)
	needMem := max64(0, p.MemBytes-t.FreeMem)

	// Pick up to K best victims on target (coverage first, then lower prio, then smaller)
	victims := pickVictimsKHop(t, prioLimit, needCPU, needMem, kVictimsPerLevel)
	if len(victims) == 0 {
		return false
	}

	// Candidate destinations: top by free (CPU then MEM)
	dests := topKDestsByFreeSnap(snap, kDestsPerLevel)

	startLen := len(*plan)
	for _, v := range victims {
		if visited[v.UID] {
			continue
		}

		for _, dn := range dests {
			if dn.Name == target && !(t.FreeCPUm+v.CPUm >= p.CPUm && t.FreeMem+v.MemBytes >= p.MemBytes) {
				continue
			}

			// If destination already fits victim, try direct move
			if snap.nodes[dn.Name].fits(v.CPUm, v.MemBytes) {
				if doMove(snap, v.UID, t.Name, dn.Name, plan) {
					if snap.nodes[target].fits(p.CPUm, p.MemBytes) {
						return true
					}
					if dfsKHopFree(snap, target, p, prioLimit, depth-(len(*plan)-startLen), visited, plan) {
						return true
					}
					undoLastMove(snap, plan)
				}
				continue
			}

			// Otherwise, free destination first (reserve at least 1 move for victim later)
			if depth-(len(*plan)-startLen) <= 1 {
				continue
			}
			visited[v.UID] = true
			mark := len(*plan)
			if dfsKHopFree(snap, dn.Name, v, prioLimit, depth-1, visited, plan) &&
				snap.nodes[dn.Name].fits(v.CPUm, v.MemBytes) &&
				doMove(snap, v.UID, t.Name, dn.Name, plan) {

				if snap.nodes[target].fits(p.CPUm, p.MemBytes) {
					visited[v.UID] = false
					return true
				}
				if dfsKHopFree(snap, target, p, prioLimit, depth-(len(*plan)-startLen), visited, plan) {
					visited[v.UID] = false
					return true
				}
				undoLastMove(snap, plan)
			}
			// backtrack destination frees
			for len(*plan) > mark {
				undoLastMove(snap, plan)
			}
			visited[v.UID] = false
		}
	}
	return false
}

/* ----------------------------- Helpers & scoring ----------------------------- */

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

func selectDirectFit(nodes map[string]*nodeState, order []*nodeState, p *podState) (string, bool) {
	bestNode := ""
	bestWaste := int64(math.MaxInt64) // minimize leftover CPU waste; tie-break on MEM
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
	if K <= 0 || K >= len(ts) {
		return ts
	}
	return ts[:K]
}

func totalFreeResources(order []*nodeState) (int64, int64) {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMem
	}
	return cpu, mem
}

/* --- snapshot for backtracking --- */

type snapshot struct {
	nodes map[string]*nodeState
	pods  map[string]*podState
}

func newSnapshot(nodes map[string]*nodeState, pods map[string]*podState) *snapshot {
	ns := make(map[string]*nodeState, len(nodes))
	for name, n := range nodes {
		c := &nodeState{
			Name:     n.Name,
			CapCPUm:  n.CapCPUm,
			CapMem:   n.CapMem,
			FreeCPUm: n.FreeCPUm,
			FreeMem:  n.FreeMem,
			Pods:     make(map[string]*podState, len(n.Pods)),
		}
		ns[name] = c
	}
	ps := make(map[string]*podState, len(pods))
	for uid, p := range pods {
		cp := *p
		ps[uid] = &cp
		if p.Node != "" {
			ns[p.Node].Pods[uid] = ps[uid]
		}
	}
	return &snapshot{nodes: ns, pods: ps}
}

func (s *snapshot) move(uid, from, to string) bool {
	p := s.pods[uid]
	if p == nil {
		return false
	}
	fn := s.nodes[from]
	tn := s.nodes[to]
	if fn == nil || tn == nil {
		return false
	}
	if fn.Pods[uid] == nil {
		return false
	}
	if !tn.fits(p.CPUm, p.MemBytes) {
		return false
	}
	fn.removePod(p)
	tn.addPod(p)
	return true
}

func doMove(snap *snapshot, uid, from, to string, plan *[]moveAction) bool {
	if snap.move(uid, from, to) {
		*plan = append(*plan, moveAction{UID: uid, From: from, To: to})
		return true
	}
	return false
}

func undoLastMove(snap *snapshot, plan *[]moveAction) {
	if len(*plan) == 0 {
		return
	}
	last := (*plan)[len(*plan)-1]
	_ = snap.move(last.UID, last.To, last.From)
	*plan = (*plan)[:len(*plan)-1]
}

func topKDestsByFreeSnap(snap *snapshot, K int) []*nodeState {
	ns := make([]*nodeState, 0, len(snap.nodes))
	for _, n := range snap.nodes {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].FreeCPUm != ns[j].FreeCPUm {
			return ns[i].FreeCPUm > ns[j].FreeCPUm
		}
		return ns[i].FreeMem > ns[j].FreeMem
	})
	if K > 0 && K < len(ns) {
		return ns[:K]
	}
	return ns
}

/* --- victim/destination selection --- */

func pickVictimsKHop(t *nodeState, prioLimit int, needCPU, needMem int64, K int) []*podState {
	type vic struct {
		p     *podState
		score int64 // coverage-weighted
	}
	buf := make([]vic, 0, len(t.Pods))
	for _, rp := range t.Pods {
		if rp.Protected || int(rp.Priority) >= prioLimit { // strictly lower prio than pending
			continue
		}
		cg := min64(rp.CPUm, needCPU)
		mg := min64(rp.MemBytes, needMem)
		sc := cg*3 + mg*2
		buf = append(buf, vic{p: rp, score: sc})
	}
	if len(buf) == 0 {
		return nil
	}
	sort.Slice(buf, func(i, j int) bool {
		if buf[i].score != buf[j].score {
			return buf[i].score > buf[j].score
		} // more coverage first
		if buf[i].p.Priority != buf[j].p.Priority {
			return buf[i].p.Priority < buf[j].p.Priority
		} // lower prio first
		if buf[i].p.CPUm != buf[j].p.CPUm {
			return buf[i].p.CPUm < buf[j].p.CPUm
		} // smaller first
		return buf[i].p.MemBytes < buf[j].p.MemBytes
	})
	if K > 0 && K < len(buf) {
		buf = buf[:K]
	}
	out := make([]*podState, len(buf))
	for i := range buf {
		out[i] = buf[i].p
	}
	return out
}

/* --- apply moves to real state & scoring --- */

func applyMovesAndRecord(nodes map[string]*nodeState, pods map[string]*podState, moves []moveAction, placements map[string]string) {
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
		to.addPod(p)
		placements[mv.UID] = to.Name
	}
}

// pickSingleEvictionCandidate: choose exactly one victim whose eviction lets p fit (global cheapest).
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
			continue
		} // already fits

		// Eligible victims: strictly lower prio, not protected
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

func buildScoreFromState(allPods map[string]*podState, originalNode map[string]string, evicted []SolverEviction) Score {
	placedByPri := map[string]int{}
	evictedSet := make(map[string]bool, len(evicted))
	for _, e := range evicted {
		evictedSet[e.UID] = true
	}

	for _, p := range allPods {
		if evictedSet[p.UID] {
			continue
		}
		if p.Node != "" {
			pr := strconv.Itoa(int(p.Priority))
			placedByPri[pr] = placedByPri[pr] + 1
		}
	}
	moves := 0
	for uid, p := range allPods {
		from := originalNode[uid]
		to := p.Node
		if from != "" && to != "" && from != to && !evictedSet[uid] {
			moves++
		}
	}

	return Score{PlacedByPriority: placedByPri, Evicted: len(evicted), Moved: moves}
}
