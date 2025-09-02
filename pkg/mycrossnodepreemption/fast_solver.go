// pkg/mycrossnodepreemption/fast_solver.go
package mycrossnodepreemption

import "sort"

// runFastSolver: simple greedy, priority-first.
// For each pending pod (sorted by priority desc, then demand desc):
//   1) place directly on a node that fits (pick the node with most remaining resources after placement);
//   2) else try ONE move: move a strictly lower-priority pod off the best candidate node to any other node;
//   3) else try ONE eviction: evict one strictly lower-priority pod on a node so the pending pod fits.
// Produces a feasible (not necessarily optimal) plan, minimizing disruption by doing at most one move or one eviction per pending pod.
func runFastSolver(in SolverInput) *SolverOutput {
	if len(in.Nodes) == 0 {
		return &SolverOutput{Status: "UNKNOWN", Placements: map[string]string{}}
	}

	// ---- nodes / capacities ----
	type cap struct{ cpu, mem int64 }
	N := len(in.Nodes)
	nodeIdx := make(map[string]int, N)
	free := make([]cap, N)
	for j, n := range in.Nodes {
		nodeIdx[n.Name] = j
		free[j] = cap{cpu: n.CPUm, mem: n.MemBytes}
	}

	// ---- pods (running + pending) ----
	type podRec struct {
		uid      string
		ns, name string
		cpu, mem int64
		pri      int32
		where    int  // -1 pending, else node index
		prot     bool // protected from eviction
	}
	P := len(in.Pods)
	pods := make([]podRec, 0, P)
	for _, p := range in.Pods {
		where := -1
		if p.Where != "" {
			if j, ok := nodeIdx[p.Where]; ok {
				where = j
			}
		}
		pods = append(pods, podRec{
			uid: p.UID, ns: p.Namespace, name: p.Name,
			cpu: p.CPU_m, mem: p.MemBytes, pri: p.Priority,
			where: where, prot: p.Protected,
		})
	}

	// subtract usage of running pods from node free capacity
	for _, pr := range pods {
		if pr.where >= 0 {
			j := pr.where
			free[j] = cap{cpu: free[j].cpu - pr.cpu, mem: free[j].mem - pr.mem}
		}
	}

	// helpers
	fits := func(j int, cpu, mem int64) bool {
		return free[j].cpu >= cpu && free[j].mem >= mem
	}
	// Best node to place (most remaining after placement, to avoid fragmentation)
	bestNodeFor := func(cpu, mem int64) (int, bool) {
		bestJ := -1
		var bestRem cap
		found := false
		for j := 0; j < N; j++ {
			if fits(j, cpu, mem) {
				rem := cap{cpu: free[j].cpu - cpu, mem: free[j].mem - mem}
				if !found || rem.cpu > bestRem.cpu || (rem.cpu == bestRem.cpu && rem.mem > bestRem.mem) {
					bestJ, bestRem, found = j, rem, true
				}
			}
		}
		return bestJ, found
	}

	placeOn := func(uid string, j int, cpu, mem int64, placements map[string]string) {
		placements[uid] = in.Nodes[j].Name
		free[j] = cap{cpu: free[j].cpu - cpu, mem: free[j].mem - mem}
	}

	placements := map[string]string{} // uid -> nodeName (includes moves + new placements)
	evictions := []SolverEviction{}
	moved := 0
	evicted := 0

	// pending indices sorted: priority desc, then (cpu+mem) desc, then uid asc (stable-ish)
	pending := make([]int, 0, P)
	for i := range pods {
		if pods[i].where < 0 {
			pending = append(pending, i)
		}
	}
	sort.Slice(pending, func(a, b int) bool {
		ia, ib := pending[a], pending[b]
		pa, pb := pods[ia], pods[ib]
		if pa.pri != pb.pri {
			return pa.pri > pb.pri
		}
		da := pa.cpu + pa.mem
		db := pb.cpu + pb.mem
		if da != db {
			return da > db
		}
		return pa.uid < pb.uid
	})

	// Try moving one strictly-lower-priority pod off node j so p can fit.
	tryOneMove := func(p podRec, j int) bool {
		// collect candidates on node j with strictly lower priority (favor lowest pri, then smallest size)
		type cand struct {
			idx      int
			cpu, mem int64
			pri      int32
		}
		cands := make([]cand, 0, 8)
		for qi := range pods {
			q := pods[qi]
			if q.where != j || q.prot || q.pri >= p.pri {
				continue
			}
			cands = append(cands, cand{idx: qi, cpu: q.cpu, mem: q.mem, pri: q.pri})
		}
		if len(cands) == 0 {
			return false
		}
		sort.Slice(cands, func(a, b int) bool {
			if cands[a].pri != cands[b].pri {
				return cands[a].pri < cands[b].pri // lower pri first
			}
			da := cands[a].cpu + cands[a].mem
			db := cands[b].cpu + cands[b].mem
			if da != db {
				return da < db // smaller first
			}
			return pods[cands[a].idx].uid < pods[cands[b].idx].uid
		})

		for _, c := range cands {
			iq := c.idx
			q := pods[iq]
			// find destination (not j) for q
			destQ, ok := bestNodeFor(q.cpu, q.mem)
			if !ok || destQ == j {
				// search manually for any other node
				ok = false
				for jj := 0; jj < N; jj++ {
					if jj == j {
						continue
					}
					if fits(jj, q.cpu, q.mem) {
						destQ = jj
						ok = true
						break
					}
				}
			}
			if !ok {
				continue
			}
			// does moving q free enough on j?
			if free[j].cpu+q.cpu >= p.cpu && free[j].mem+q.mem >= p.mem {
				// apply move q: j -> destQ
				placeOn(q.uid, destQ, q.cpu, q.mem, placements)
				// free capacity on j (since q left)
				free[j] = cap{cpu: free[j].cpu + q.cpu, mem: free[j].mem + q.mem}
				pods[iq].where = destQ
				moved++

				// place p on j
				placeOn(p.uid, j, p.cpu, p.mem, placements)
				return true
			}
		}
		return false
	}

	// Evict one strictly-lower-priority pod so p can fit on *some* node.
	tryOneEvict := func(p podRec) bool {
		type choice struct {
			j    int
			idx  int
			pri  int32
			size int64
		}
		var pick *choice
		for j := 0; j < N; j++ {
			for qi := range pods {
				q := pods[qi]
				if q.where != j || q.prot || q.pri >= p.pri {
					continue
				}
				// after evicting q, will p fit on j?
				if free[j].cpu+q.cpu >= p.cpu && free[j].mem+q.mem >= p.mem {
					cand := choice{j: j, idx: qi, pri: q.pri, size: q.cpu + q.mem}
					if pick == nil ||
						cand.pri < pick.pri || // strictly lower priority first
						(cand.pri == pick.pri && cand.size < pick.size) {
						tmp := cand
						pick = &tmp
					}
				}
			}
		}
		if pick == nil {
			return false
		}
		// apply eviction
		q := pods[pick.idx]
		evictions = append(evictions, SolverEviction{UID: q.uid, Namespace: q.ns, Name: q.name})
		// free up its resources
		free[pick.j] = cap{cpu: free[pick.j].cpu + q.cpu, mem: free[pick.j].mem + q.mem}
		pods[pick.idx].where = -1
		evicted++

		// place p on that node
		placeOn(p.uid, pick.j, p.cpu, p.mem, placements)
		return true
	}

	// nodes sorted by "deficit" for p (closest to fitting) to try move-first where it's most promising
	type nodeTry struct {
		j              int
		defCPU, defMem int64
	}
	orderNodesFor := func(p podRec) []int {
		arr := make([]nodeTry, 0, N)
		for j := 0; j < N; j++ {
			needCPU := p.cpu - free[j].cpu
			if needCPU < 0 {
				needCPU = 0
			}
			needMem := p.mem - free[j].mem
			if needMem < 0 {
				needMem = 0
			}
			arr = append(arr, nodeTry{j: j, defCPU: needCPU, defMem: needMem})
		}
		sort.Slice(arr, func(a, b int) bool {
			if arr[a].defCPU != arr[b].defCPU {
				return arr[a].defCPU < arr[b].defCPU
			}
			return arr[a].defMem < arr[b].defMem
		})
		out := make([]int, 0, N)
		for _, e := range arr {
			out = append(out, e.j)
		}
		return out
	}

	// ---- main loop over pending pods ----
	for _, ip := range pending {
		p := pods[ip]

		// 1) direct
		if j, ok := bestNodeFor(p.cpu, p.mem); ok {
			placeOn(p.uid, j, p.cpu, p.mem, placements)
			continue
		}

		// 2) one move
		triedMove := false
		for _, j := range orderNodesFor(p) {
			if tryOneMove(p, j) {
				triedMove = true
				break
			}
		}
		if triedMove {
			continue
		}

		// 3) one eviction
		_ = tryOneEvict(p)
	}

	// ---- score (lexi-compatible) ----
	placedByPr := map[string]int{}
	// count all currently running after our virtual changes (pods[].where >= 0)
	for _, pr := range pods {
		if pr.where >= 0 {
			placedByPr[int32ToStr(pr.pri)]++
		}
	}
	// plus pending we placed now (tracked only in placements map)
	for _, ip := range pending {
		p := pods[ip]
		if nodeName, ok := placements[p.uid]; ok && nodeName != "" {
			placedByPr[int32ToStr(p.pri)]++
		}
	}

	score := Score{
		PlacedByPriority: placedByPr,
		Evicted:          evicted,
		Moved:            moved,
	}

	// nominated preemptor (single-preemptor mode)
	nominated := ""
	if in.Preemptor != nil {
		if node, ok := placements[in.Preemptor.UID]; ok {
			nominated = node
		}
	}

	return &SolverOutput{
		Status:        "FEASIBLE",
		NominatedNode: nominated,
		Placements:    placements,
		Evictions:     evictions,
		Score:         score,
	}
}

func int32ToStr(x int32) string {
	if x == 0 {
		return "0"
	}
	sign := ""
	if x < 0 {
		sign = "-"
		x = -x
	}
	var buf [12]byte
	i := len(buf)
	for x > 0 {
		i--
		buf[i] = byte('0' + (x % 10))
		x /= 10
	}
	if sign != "" {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
