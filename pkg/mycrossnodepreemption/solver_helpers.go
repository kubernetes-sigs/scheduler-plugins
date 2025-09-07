package mycrossnodepreemption

import (
	"math"
	"sort"

	"k8s.io/klog/v2"
)

type pLite struct {
	// Unique identifier of the pod
	UID string
	// Requested CPU in milliCPU
	CPUm int64
	// Requested memory in bytes
	MemBytes int64
	// Priority of the pod
	Priority int32
	// Whether the pod is protected
	Protected bool
	// Current node of the pod; "" if pending
	Node string // "" if pending
	// Original node of the pod
	origNode string
}

type nLite struct {
	// Name of the node
	Name string
	// Total capacity on the node of milliCPU
	CapCPUm int64
	// Total capacity on the node of memory in bytes
	CapMemBytes int64
	// Free capacity on the node of milliCPU
	FreeCPUm int64
	// Free capacity on the node of memory in bytes
	FreeMemBytes int64
	// Pods on the node
	Pods map[string]*pLite
}

type moveLite struct{ UID, From, To string }

type UIDSet map[string]struct{}

func (s UIDSet) Add(uid string)      { s[uid] = struct{}{} }
func (s UIDSet) Del(uid string)      { delete(s, uid) }
func (s UIDSet) Has(uid string) bool { _, ok := s[uid]; return ok }

// bestPlanAcrossTargets iterates targets (ordered by deficit for p) and
// keeps the plan with the fewest moves. `planForTarget` should return the
// candidate move list for that target (or !ok if no plan exists).
func bestPlanAcrossTargets(
	p *pLite,
	order []*nLite,
	planForTarget func(t *nLite) (moves []moveLite, ok bool),
) (bestMoves []moveLite, bestTarget string, ok bool) {
	bestCount := math.MaxInt32
	for _, t := range orderTargetsByDeficit(order, p) {
		mvs, okT := planForTarget(t)
		if !okT {
			continue
		}
		if len(mvs) < bestCount {
			bestCount, bestMoves, bestTarget, ok = len(mvs), mvs, t.Name, true
			if bestCount <= 1 { // can’t beat 0/1
				break
			}
		}
	}
	return
}

// gateAllows centralizes priority checks (strict: < vs <=).
func gateAllows(p *pLite, gate *int32, strict bool) bool {
	if p == nil || p.Protected {
		return false
	}
	if gate == nil {
		return true
	}
	if strict {
		return p.Priority < *gate
	}
	return p.Priority <= *gate
}

func canMove(p *pLite, gate *int32) bool {
	if p == nil || p.Node == "" {
		return false
	}
	return gateAllows(p, gate, false)
}

func canEvict(p *pLite, gate *int32) bool {
	return gateAllows(p, gate, true)
}

func addNodeDelta(m map[string]Delta, node string, dcpu, dmem int64) {
	d := m[node]
	d.cpu += dcpu
	d.mem += dmem
	m[node] = d
}

// Adds +cpu/+mem to "from" and −cpu/−mem to "to".
func addEdgeDelta(m map[string]Delta, from, to string, cpu, mem int64) {
	addNodeDelta(m, from, +cpu, +mem)
	addNodeDelta(m, to, -cpu, -mem)
}

type tryRelocate func(
	p *pLite,
	nodes map[string]*nLite,
	pods map[string]*pLite,
	order []*nLite,
	moveGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
) bool

func runSolverCommon(in SolverInput, tryRelocate tryRelocate, tag string) *SolverOutput {
	nodes, pods, pending, order, pre := buildClusterState(in)

	if len(pending) == 0 {
		klog.InfoS(tag + ": nothing to place")
		return &SolverOutput{Status: "UNKNOWN", Placements: nil, Evictions: nil}
	}

	// Worklist (big-first) and mode flags (single vs batch)
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

		placed, infeasible := placeOnePodCommon(
			p, nodes, pods, order,
			moveGatePtr,
			evictGateForPod(p, singleMode, pre),
			movedUIDs, newPlacements, &evicts,
			tryRelocate,
		)

		if !placed {
			if singleMode || infeasible {
				return stableOutput("INFEASIBLE", newPlacements, evicts, in)
			}
			stop = true
			stopAt = p.Priority
			klog.InfoS(tag+": stopping batch at priority; discarding remainder",
				"priority", stopAt, "uid", p.UID)
		}
	}

	return stableOutput("FEASIBLE", newPlacements, evicts, in)
}

// commitPlanAndPlace verifies & applies `moves`, records them in newPlacements/movedUIDs,
// then places p on `target` (if it fits). If not, it falls back to bestDirectFit.
// Returns true on success, false if the plan is invalid or placement fails.
func commitPlanAndPlace(
	p *pLite,
	target string,
	moves []moveLite,
	nodes map[string]*nLite,
	pods map[string]*pLite,
	order []*nLite,
	newPlacements map[string]string,
	movedUIDs map[string]struct{},
) bool {
	if !verifyPlan(nodes, pods, moves) {
		return false
	}
	for _, mv := range moves {
		newPlacements[mv.UID] = mv.To
		movedUIDs[mv.UID] = struct{}{}
	}
	if n := nodes[target]; n != nil && n.canPodFit(p.CPUm, p.MemBytes) {
		n.addPod(p)
		newPlacements[p.UID] = target
		return true
	}
	// Defensive fallback: best-fit anywhere (in case of tiny drift)
	if to, ok := bestDirectFit(order, p); ok {
		nodes[to].addPod(p)
		newPlacements[p.UID] = to
		return true
	}
	return false
}

func placeOnePodCommon(
	p *pLite,
	nodes map[string]*nLite,
	pods map[string]*pLite,
	order []*nLite,
	moveGate *int32,
	evictGate *int32,
	movedUIDs map[string]struct{},
	newPlacements map[string]string,
	evicts *[]Placement,
	tryRelocate tryRelocate,
) (feasible bool, triedEvicting bool) {

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

	// 3) Moves-only via strategy hook (Swap virtual planner or BFS)
	if tryRelocate(p, nodes, pods, order, moveGate, movedUIDs, newPlacements) {
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
	on.removePod(v)
	*evicts = append(*evicts, Placement{Pod: Pod{UID: v.UID}, Node: on.Name})

	if on.canPodFit(p.CPUm, p.MemBytes) {
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

// buildClusterState builds the cluster state from the given solver input.
// It returns:
//   - map of node name → *nLite
//   - map of pod UID → *pLite
//   - slice of pending pods (to be scheduled)
//   - slice of all nodes in lexicographical order by name
//   - the preemptor pod if any (nil otherwise)
func buildClusterState(in SolverInput) (map[string]*nLite, map[string]*pLite, []*pLite, []*nLite, *pLite) {
	// Build nodes map
	nodes := make(map[string]*nLite, len(in.Nodes))
	order := make([]*nLite, 0, len(in.Nodes))
	for i := range in.Nodes {
		n := &nLite{
			Name:         in.Nodes[i].Name,
			CapCPUm:      in.Nodes[i].CPUm,
			CapMemBytes:  in.Nodes[i].MemBytes,
			FreeCPUm:     in.Nodes[i].CPUm,
			FreeMemBytes: in.Nodes[i].MemBytes,
			Pods:         make(map[string]*pLite, 32),
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })

	// Build pods map and assign pods to nodes
	pods := make(map[string]*pLite, len(in.Pods)+1)
	pendingPods := make([]*pLite, 0, len(in.Pods))
	var pre *pLite
	// Add the preemptor to the total set of pods if it exists
	if in.Preemptor != nil {
		pendingPods = append(pendingPods, &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		})
		pre = &pLite{
			UID:       in.Preemptor.UID,
			CPUm:      in.Preemptor.CPU_m,
			MemBytes:  in.Preemptor.MemBytes,
			Priority:  in.Preemptor.Priority,
			Protected: in.Preemptor.Protected,
		}
		pods[pre.UID] = pre
	}
	// Add also other pods to the total set of pods
	for i := range in.Pods {
		sp := in.Pods[i]
		if sp.Where == "" { // pending => treat as incoming
			pendingPods = append(pendingPods, &pLite{
				UID:       sp.UID,
				CPUm:      sp.CPU_m,
				MemBytes:  sp.MemBytes,
				Priority:  sp.Priority,
				Protected: sp.Protected,
			})
		}
		p := &pLite{
			UID:       sp.UID,
			CPUm:      sp.CPU_m,
			MemBytes:  sp.MemBytes,
			Priority:  sp.Priority,
			Protected: sp.Protected,
			Node:      sp.Where,
			origNode:  sp.Where,
		}
		pods[p.UID] = p

		// Add the pod to its node
		if p.Node != "" {
			if node := nodes[p.Node]; node != nil {
				node.addPod(p)
			}
		}
	}

	return nodes, pods, pendingPods, order, pre
}

// max64 returns the larger of a or b.
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// min64 returns the smaller of a or b.
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// stableOutput produces a stable SolverOutput from the given status, placements map, evictions list, and input.
// The placements map is from pod UID to node name ("" means no placement).
// The evictions list is a list of Placement structs indicating which pods to evict.
// The output is stable in that the Placements slice is sorted by pod UID ascending,
// and within that, the pods are looked up by UID from the input to get their Namespace and Name.
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

// pickLargestEnablingEviction picks the best pod to evict to enable placement of p.
// It returns the pod to evict and the node it’s on, or nil, nil if no such pod exists.
// The eviction gate is used to restrict which pods can be considered for eviction:
// only pods with Priority < *evictGate can be considered (while still honoring `Protected`).
// If evictGate is nil, there is no priority restriction (all non-protected pods can be considered).
// The selection criteria are:
//  1. The eviction must enable direct placement of p on the pod’s node.
//  2. Among all pods that satisfy (1), keep only those with the lowest priority.
//  3. Among those, prefer pods that have already been moved in this cycle.
//  4. Among those, pick the largest (CPUm*MemBytes), then by CPU, then by MEM.
//  5. Among those, pick lexicographically by node name, then by pod UID.
func pickLargestEnablingEviction(order []*nLite, p *pLite, evictGate *int32, movedUIDs map[string]struct{}) (*pLite, *nLite) {
	type cand struct {
		v  *pLite
		on *nLite
	}
	cands := make([]cand, 0, 64)
	minPrioSet := false
	var minPrio int32

	// Build enabling candidates and track the minimum priority among them.
	for _, n := range order {
		for _, q := range n.Pods {
			if !canEvict(q, evictGate) {
				continue // enforces q.Priority < gate (i.e., < p.Priority in batch)
			}
			// enabling: evicting q must allow direct placement of p on n
			if n.FreeCPUm+q.CPUm >= p.CPUm && n.FreeMemBytes+q.MemBytes >= p.MemBytes {
				cands = append(cands, cand{v: q, on: n})
				if !minPrioSet || q.Priority < minPrio {
					minPrio = q.Priority
					minPrioSet = true
				}
			}
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// Keep only the lowest-priority tier.
	kept := cands[:0]
	for _, c := range cands {
		if c.v.Priority == minPrio {
			kept = append(kept, c)
		}
	}
	cands = kept

	// Sort within the lowest-priority tier:
	// alreadyMoved → size desc → CPU desc → MEM desc → node name asc → UID asc
	sort.Slice(cands, func(i, j int) bool {
		vi, vj := cands[i].v, cands[j].v

		mi := hasKey(movedUIDs, vi.UID)
		mj := hasKey(movedUIDs, vj.UID)
		if mi != mj {
			return mi // prefer already moved
		}

		si, sj := vi.CPUm*vi.MemBytes, vj.CPUm*vj.MemBytes
		if si != sj {
			return si > sj // larger first
		}
		if vi.CPUm != vj.CPUm {
			return vi.CPUm > vj.CPUm
		}
		if vi.MemBytes != vj.MemBytes {
			return vi.MemBytes > vj.MemBytes
		}
		if cands[i].on.Name != cands[j].on.Name {
			return cands[i].on.Name < cands[j].on.Name
		}
		return vi.UID < vj.UID
	})

	return cands[0].v, cands[0].on
}

// hasKey reports whether map m has key k.
func hasKey(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

// buildOrigMap returns UID -> current node for all placed pods.
func buildOrigMap(order []*nLite) map[string]string {
	orig := make(map[string]string, 256)
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Node != "" {
				orig[q.UID] = q.Node
			}
		}
	}
	return orig
}

type targetScore struct {
	n      *nLite
	score  float64 // max(defCPU/p.CPU, defMEM/p.MEM)
	defSum int64
	waste  int64
}

// orderTargetsByDeficit orders nodes by how well they can accommodate pod p,
// even if they can’t fit it directly.
// The ordering is:
//  1. score ASC (lower is better)
//  2. defSum ASC (lower is better)
//  3. waste ASC (lower is better)
//  4. name ASC (lexicographically)
func orderTargetsByDeficit(order []*nLite, p *pLite) []*nLite {
	s := make([]targetScore, 0, len(order))
	for _, n := range order {
		defCPU := max64(0, p.CPUm-n.FreeCPUm)
		defMEM := max64(0, p.MemBytes-n.FreeMemBytes)
		score := float64(max64(
			int64(float64(defCPU)/float64(max64(1, p.CPUm))*1_000_000),
			int64(float64(defMEM)/float64(max64(1, p.MemBytes))*1_000_000),
		)) / 1_000_000.0
		waste := int64(0)
		if n.canPodFit(p.CPUm, p.MemBytes) {
			waste = (n.FreeCPUm - p.CPUm) + (n.FreeMemBytes - p.MemBytes)
		}
		s = append(s, targetScore{n: n, score: score, defSum: defCPU + defMEM, waste: waste})
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].score != s[j].score {
			return s[i].score < s[j].score
		}
		if s[i].defSum != s[j].defSum {
			return s[i].defSum < s[j].defSum
		}
		if s[i].waste != s[j].waste {
			return s[i].waste < s[j].waste
		}
		return s[i].n.Name < s[j].n.Name
	})
	out := make([]*nLite, 0, len(s))
	for _, e := range s {
		out = append(out, e.n)
	}
	return out
}

// bestDirectFit finds the best-fit node for pod p in order.
// It returns the node name and true if found, or "", false if not found.
// Best-fit is defined as the node that minimizes CPU waste, then MEM waste,
// then lexicographically by node name.
func bestDirectFit(order []*nLite, p *pLite) (string, bool) {
	bestNode := ""
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.canPodFit(p.CPUm, p.MemBytes) {
			cw := n.FreeCPUm - p.CPUm
			mw := n.FreeMemBytes - p.MemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && n.Name < bestNode))) {
				bestNode, bestCPUWaste, bestMEMWaste = n.Name, cw, mw
			}
		}
	}
	return bestNode, bestNode != ""
}

// clusterHasSlack returns true iff the cluster has total enough free resources to potentially fit p.
// The pod p may not fit on any single node, but if clusterHasSlack returns false, it means the cluster is
// unable to accommodate p even with all resources considered.
func clusterHasSlack(order []*nLite, p *pLite) bool {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMemBytes
	}
	return cpu >= p.CPUm && mem >= p.MemBytes
}

// evictGateForPod returns the eviction gate for pod p.
// In single-preemptor mode, the gate is the preemptor’s priority;
// in batch mode, it’s p.Priority.
func evictGateForPod(p *pLite, single bool, pre *pLite) *int32 {
	if single && pre != nil {
		eg := pre.Priority
		return &eg
	}
	eg := p.Priority
	return &eg
}

// buildWorklist constructs the scheduling worklist for a solver and decides
// whether we’re in **single-preemptor** mode or **batch** mode.
//
// Modes
//   - Single-preemptor mode: If `pre` is present, we return a slice containing
//     only that pod, set `single=true`, and return `moveGate=&pre.Priority`.
//     The move gate is used downstream to restrict relocations so that only
//     pods with Priority ≤ *moveGate can be moved (while still honoring `Protected`).
//     This matches the “don’t move pods above the preemptor’s priority” rule.
//   - Batch mode: Otherwise we return **all** pending pods ordered big-first,
//     `single=false`, and `moveGate=nil` (meaning moves are not priority-gated,
//     still respecting `Protected`).
//
// Ordering (batch mode)
//
//	We sort pending pods to reduce fragmentation and front-load hard placements:
//	  1) Priority DESC (higher first)
//	  2) Size (CPUm*MemBytes) DESC
//	  3) CPUm DESC
//	  4) MemBytes DESC
//	  5) UID ASC (stable tie-breaker for determinism)
//
// Returns
//
//	out:      ordered list of pods to try placing this cycle
//	single:   true iff we’re in single-preemptor mode
//	moveGate: pointer to the priority threshold for moves (non-nil in single
//	          mode; nil in batch mode). The pointer is safe to return—Go will
//	          heap-allocate `mg` as needed.
func buildWorklist(pending []*pLite, pre *pLite) (out []*pLite, single bool, moveGate *int32) {
	if pre != nil {
		for _, p := range pending {
			if p.UID == pre.UID {
				mg := pre.Priority
				return []*pLite{p}, true, &mg
			}
		}
	}
	out = append(out, pending...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		sa, sb := a.CPUm*a.MemBytes, b.CPUm*b.MemBytes
		if sa != sb {
			return sa > sb
		}
		if a.CPUm != b.CPUm {
			return a.CPUm > b.CPUm
		}
		if a.MemBytes != b.MemBytes {
			return a.MemBytes > b.MemBytes
		}
		return a.UID < b.UID
	})
	return out, false, nil
}

// verifyPlan checks that the proposed plan is valid and applies it to the nodes/pods state.
// It returns true if the plan was valid and applied, false otherwise.
// The plan is valid if:
//   - all moves are valid (pods exist, source/destination nodes exist, pod is on source node, pod is not on destination node, source != destination)
//   - no node ends up with negative free resources after all moves are applied
//
// If the plan is valid, it is applied in-place to the nodes and pods state.
func verifyPlan(nodes map[string]*nLite, all map[string]*pLite, moves []moveLite) bool {
	if len(moves) == 0 {
		return true
	}

	// 1) Validate & compute final per-node deltas (must not go negative).
	type dm struct{ cpu, mem int64 }
	per := make(map[string]dm, 16)

	for i := range moves {
		mv := moves[i]
		p := all[mv.UID]
		src, dst := nodes[mv.From], nodes[mv.To]

		// basic endpoint checks + duplicate/no-op guards
		if p == nil || src == nil || dst == nil || mv.From == mv.To || src.Pods[p.UID] == nil || dst.Pods[p.UID] != nil {
			klog.InfoS("apply: invalid move", "i", i, "uid", mv.UID, "from", mv.From, "to", mv.To)
			return false
		}

		// accumulate net delta
		df := per[src.Name]
		df.cpu += p.CPUm
		df.mem += p.MemBytes
		per[src.Name] = df

		dt := per[dst.Name]
		dt.cpu -= p.CPUm
		dt.mem -= p.MemBytes
		per[dst.Name] = dt
	}

	for name, dd := range per {
		n := nodes[name]
		if n.FreeCPUm+dd.cpu < 0 || n.FreeMemBytes+dd.mem < 0 {
			klog.InfoS("apply: reject, final negative free",
				"node", name, "freeCPU_now", n.FreeCPUm, "freeMem_now", n.FreeMemBytes,
				"deltaCPU", dd.cpu, "deltaMem", dd.mem,
				"finalCPU", n.FreeCPUm+dd.cpu, "finalMem", n.FreeMemBytes+dd.mem)
			return false
		}
	}

	// 2) Remove from sources.
	for _, mv := range moves {
		if p := all[mv.UID]; p != nil {
			if n := nodes[mv.From]; n != nil && n.Pods[p.UID] != nil {
				n.removePod(p)
			}
		}
	}

	// 3) Add to destinations (now guaranteed to fit).
	for _, mv := range moves {
		p := all[mv.UID]
		n := nodes[mv.To]
		if !n.canPodFit(p.CPUm, p.MemBytes) {
			klog.InfoS("apply: unexpected no-fit at destination", "uid", p.UID, "to", n.Name)
			return false
		}
		n.addPod(p)
	}

	return true
}

// canPodFit returns true iff the node has enough free resources to fit the given cpu/mem request.
func (n *nLite) canPodFit(cpu, mem int64) bool { return n.FreeCPUm >= cpu && n.FreeMemBytes >= mem }

// addPod adds pod p to node n, updating free resources accordingly.
func (n *nLite) addPod(p *pLite) {
	n.FreeCPUm -= p.CPUm
	n.FreeMemBytes -= p.MemBytes
	n.Pods[p.UID] = p
	p.Node = n.Name
}

// removePod removes pod p from node n, updating free resources accordingly.
func (n *nLite) removePod(p *pLite) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.FreeCPUm += p.CPUm
		n.FreeMemBytes += p.MemBytes
		p.Node = ""
	}
}
