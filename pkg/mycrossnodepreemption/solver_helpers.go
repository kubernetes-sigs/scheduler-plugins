package mycrossnodepreemption

import (
	"math"
	"sort"
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

// stableOutput converts newPlacements + evicts into your SolverOutput.
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

// -----------------------------------------------------------------------------
// Evictions: strictly-lower priority & enabling-only; largest by size
// -----------------------------------------------------------------------------

func pickLargestEnablingEviction(
	order []*nLite,
	p *pLite,
	evictGate *int32,
	movedUIDs map[string]struct{},
) (*pLite, *nLite) {
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

func hasKey(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

func canEvict(p *pLite, gate *int32) bool {
	if p == nil || p.Protected {
		return false
	}
	if gate == nil {
		return true
	}
	return p.Priority < *gate
}

type targetScore struct {
	n      *nLite
	score  float64 // max(defCPU/p.CPU, defMEM/p.MEM)
	defSum int64
	waste  int64
}

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
		if n.fits(p.CPUm, p.MemBytes) {
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

// -----------------------------------------------------------------------------
// B.2 Direct best-fit (minimize post-placement waste)
// -----------------------------------------------------------------------------

func bestDirectFit(order []*nLite, p *pLite) (string, bool) {
	bestNode := ""
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.fits(p.CPUm, p.MemBytes) {
			cw := n.FreeCPUm - p.CPUm
			mw := n.FreeMemBytes - p.MemBytes
			if cw < bestCPUWaste || (cw == bestCPUWaste && (mw < bestMEMWaste || (mw == bestMEMWaste && n.Name < bestNode))) {
				bestNode, bestCPUWaste, bestMEMWaste = n.Name, cw, mw
			}
		}
	}
	return bestNode, bestNode != ""
}

// -----------------------------------------------------------------------------
// B.1 Cluster slack
// -----------------------------------------------------------------------------

func clusterHasSlack(order []*nLite, p *pLite) bool {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMemBytes
	}
	return cpu >= p.CPUm && mem >= p.MemBytes
}

func evictGateForPod(p *pLite, single bool, pre *pLite) *int32 {
	if single && pre != nil {
		eg := pre.Priority
		return &eg
	}
	eg := p.Priority
	return &eg
}

// -----------------------------------------------------------------------------
// Worklist / gates
// -----------------------------------------------------------------------------

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
