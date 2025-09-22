// solver_helpers.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// TODO: Reach to here in this file...

// runSolverDirectFit tries to place pods by direct-fit only (no evictions, no moves).
func runSolverDirectFit(in SolverInput, base *PreparedState) *SolverOutput {
	nodes, _, order, worklist := base.freshClone()
	if len(worklist) == 0 {
		return &SolverOutput{Status: "UNKNOWN"}
	}
	placements := make(map[types.UID]string, len(worklist))

	stop := false
	var stopAt int32
	for _, p := range worklist {
		if stop && p.Priority <= stopAt {
			break
		}
		if to, ok := bestDirectFit(order, p); ok {
			nodes[to].addPod(p)
			placements[p.UID] = to
			continue
		}
		if base.Single { // single-preemptor must place or fail
			return stableOutput("INFEASIBLE", placements, nil, in)
		}
		stop = true
		stopAt = p.Priority
	}
	if len(placements) == 0 {
		return &SolverOutput{Status: "INFEASIBLE"}
	}
	return stableOutput("FEASIBLE", placements, nil, in)
}

// runSolverCommon runs a solver plan function on the input and prepared state.
func runSolverCommon(in SolverInput, plan PlanFunc, tag string, base *PreparedState) *SolverOutput {
	klog.V(MyV).InfoS("Running solver", "tag", tag)
	nodes, pods, order, worklist := base.freshClone()
	if len(worklist) == 0 {
		return &SolverOutput{Status: "UNKNOWN"}
	}

	newPlacements := make(map[types.UID]string)
	var evicts []Placement
	movedUIDs := make(map[types.UID]struct{})

	stop := false
	var stopAt int32

	maxTrials := in.MaxTrials
	if maxTrials < 1 {
		maxTrials = 1
	}

	for _, p := range worklist {
		if stop && p.Priority <= stopAt {
			break
		}
		if base.Single && base.Preemptor != nil && p.UID != base.Preemptor.UID {
			continue
		}
		placed, infeasible := placeOnePodCommon(
			plan,
			p, nodes, pods, order,
			base.MoveGate,
			evictGateForPod(p, base.Single, base.Preemptor),
			movedUIDs, newPlacements, &evicts,
			maxTrials,
		)
		if !placed {
			if base.Single || infeasible {
				return stableOutput("INFEASIBLE", newPlacements, evicts, in)
			}
			stop = true
			stopAt = p.Priority
		}
	}
	return stableOutput("FEASIBLE", newPlacements, evicts, in)
}

// exportSolverStats exports a compact run record to the stats ConfigMap.
// Only runs when `hadFeasible` is true.
func (pl *MyCrossNodePreemption) exportSolverStats(
	ctx context.Context,
	label string,
	baseline SolverScore,
	best SolverResult,
	attemptsFeasible []SolverResult,
	hadFeasible bool,
) {
	if !hadFeasible {
		return
	}
	attempts := make([]SolverResult, 0, len(attemptsFeasible))
	for _, r := range attemptsFeasible {
		attempts = append(attempts, summarizeAttempt(r))
	}
	entry := ExportedSolverStats{
		TimestampNs: time.Now().UnixNano(),
		Best:        best.Name,
		PlanStatus:  PlanStatusActive,
		Baseline:    baseline,
		Attempts:    attempts,
	}
	pl.appendStatsCM(ctx, entry)
	klog.V(MyV).InfoS(label+": exported solver stats", "attempts", len(attempts), "best", best.Name)
}

// logLeaderboard prints a compact solver leaderboard relative to baseline.
// It groups attempts as better/equal/worse vs baseline and tags adjacent ties.
func (pl *MyCrossNodePreemption) logLeaderboard(
	label string,
	attempts []SolverResult,
	baseline SolverScore,
	best SolverResult,
) {
	if len(attempts) == 0 {
		return
	}

	// Classify relative to baseline while preserving attempt order inside groups.
	var better, equal, worse []SolverResult
	for _, r := range attempts {
		rr := SolverResult{
			Name:       r.Name,
			DurationUs: r.DurationUs,
			Score:      r.Score,
			Status:     r.Status,
			CmpBase:    IsImprovement(baseline, r.Score),
		}
		switch rr.CmpBase {
		case 1:
			better = append(better, rr)
		case 0:
			equal = append(equal, rr)
		default:
			worse = append(worse, rr)
		}
	}
	ranking := append(append(better, equal...), worse...)

	// Tie helper
	tied := func(a, b SolverScore) bool {
		return IsImprovement(a, b) == 0 && IsImprovement(b, a) == 0
	}

	// Build log arrays
	names := make([]string, len(ranking))
	evictions := make([]int, len(ranking))
	moves := make([]int, len(ranking))
	durations := make([]int64, len(ranking))
	for i := range ranking {
		lbl := ranking[i].Name
		if i > 0 && tied(ranking[i-1].Score, ranking[i].Score) {
			lbl += " (tie)"
		}
		names[i] = lbl
		evictions[i] = ranking[i].Score.Evicted
		moves[i] = ranking[i].Score.Moved
		durations[i] = ranking[i].DurationUs
	}

	placed := baseline.PlacedByPriority
	if best.Name != "baseline" {
		placed = best.Score.PlacedByPriority
	}

	klog.InfoS(label+": solver leaderboard",
		"ranking", names,
		"durationsUs", durations,
		"evictions", evictions,
		"moves", moves,
		"placedByPri", placed,
	)
}

// copy to a "summary": drop Output/CmpBase and fill Status from Output.
func summarizeAttempt(r SolverResult) SolverResult {
	status := r.Status
	if status == "" && r.Output != nil {
		status = r.Output.Status
	}
	return SolverResult{
		Name:       r.Name,
		Status:     status,
		DurationUs: r.DurationUs,
		Score:      r.Score,
	}
}

// append (create if missing) an entry to the ConfigMap ledger
func (pl *MyCrossNodePreemption) appendStatsCM(ctx context.Context, entry ExportedSolverStats) {
	cli := pl.Handle.ClientSet()
	if cli == nil {
		klog.V(1).Info("no clientset; skip stats CM")
		return
	}
	cms := cli.CoreV1().ConfigMaps(SystemNamespace)
	cm, err := cms.Get(ctx, SolverConfigMapExportedStatsName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "get CM failed", "namespace", SystemNamespace, "name", SolverConfigMapExportedStatsName)
			return
		}
		// create fresh
		buf, _ := json.Marshal([]ExportedSolverStats{entry})
		cm = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SolverConfigMapExportedStatsName,
				Namespace: SystemNamespace,
				Labels:    map[string]string{SolverConfigMapLabelKey: "true"},
			},
			Data: map[string]string{SolverConfigMapLabelKey + ".json": string(buf)},
		}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			klog.ErrorS(err, "create CM failed", "namespace", SystemNamespace, "name", SolverConfigMapExportedStatsName)
		}
		return
	}
	// update existing
	var arr []ExportedSolverStats
	if s := cm.Data[SolverConfigMapLabelKey+".json"]; s != "" {
		_ = json.Unmarshal([]byte(s), &arr)
		// best-effort
	}
	arr = append(arr, entry)
	buf, _ := json.Marshal(arr)
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[SolverConfigMapLabelKey+".json"] = string(buf)
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "update CM failed", "namespace", SystemNamespace, "name", SolverConfigMapExportedStatsName)
	}
}

// bestPlanAcrossTargets iterates targets (ordered by deficit for p) and
// keeps the plan with the fewest moves. `planForTarget` should return the
// candidate move list for that target (or !ok if no plan exists).
func bestPlanAcrossTargets(
	p *SolverPod,
	order []*SolverNode,
	planForTarget func(t *SolverNode) (moves []MoveLite, ok bool),
) (bestMoves []MoveLite, bestTarget string, ok bool) {
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

// orderTargetsByDeficit orders nodes by how well they can accommodate pod p,
// even if they can’t fit it directly.
// The ordering is:
//  1. score ASC (lower is better)
//  2. defSum ASC (lower is better)
//  3. waste ASC (lower is better)
//  4. name ASC (lexicographically)
func orderTargetsByDeficit(order []*SolverNode, p *SolverPod) []*SolverNode {
	s := make([]TargetScore, 0, len(order))
	for _, n := range order {
		defCPU := max64(0, p.ReqCPUm-n.AllocCPUm)
		defMEM := max64(0, p.ReqMemBytes-n.AllocMemBytes)
		score := float64(max64(
			int64(float64(defCPU)/float64(max64(1, p.ReqCPUm))*1_000_000),
			int64(float64(defMEM)/float64(max64(1, p.ReqMemBytes))*1_000_000),
		)) / 1_000_000.0
		waste := int64(0)
		if n.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
			waste = (n.AllocCPUm - p.ReqCPUm) + (n.AllocMemBytes - p.ReqMemBytes)
		}
		s = append(s, TargetScore{Node: n, Score: score, DefSum: defCPU + defMEM, Waste: waste})
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score < s[j].Score
		}
		if s[i].DefSum != s[j].DefSum {
			return s[i].DefSum < s[j].DefSum
		}
		if s[i].Waste != s[j].Waste {
			return s[i].Waste < s[j].Waste
		}
		return s[i].Node.Name < s[j].Node.Name
	})
	out := make([]*SolverNode, 0, len(s))
	for _, e := range s {
		out = append(out, e.Node)
	}
	return out
}

// podAllowedByPriority centralizes priority checks (strict: < vs <=).
// Returns false if p is nil, protected or if gate is set and p.Priority is too high; true otherwise.
func podAllowedByPriority(p *SolverPod, gate *int32, strict bool) bool {
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

// canMove returns true if pod p can be moved (not nil, not pending, not protected, below moveGate if any).
func canMove(p *SolverPod, gate *int32) bool {
	if p == nil || p.Node == "" {
		return false
	}
	return podAllowedByPriority(p, gate, false)
}

// canEvict returns true if pod p can be evicted (not nil, not protected, below evictGate if any).
func canEvict(p *SolverPod, gate *int32) bool {
	return podAllowedByPriority(p, gate, true)
}

// addNodeDelta adds +cpu/+mem to the given node in the map.
func addNodeDelta(m map[string]Delta, node string, deficitCPU, deficitMem int64) {
	d := m[node]
	d.CPU += deficitCPU
	d.Mem += deficitMem
	m[node] = d
}

// addEdgeDelta adds +cpu/+mem to `from` and -cpu/-mem to `to` in the map.
func addEdgeDelta(m map[string]Delta, from, to string, cpu, mem int64) {
	addNodeDelta(m, from, +cpu, +mem)
	addNodeDelta(m, to, -cpu, -mem)
}

// relocateViaPlan tries to relocate pod p to target via the given plan function.
func relocateViaPlan(
	plan PlanFunc,
	p *SolverPod,
	nodes map[string]*SolverNode,
	pods map[types.UID]*SolverPod,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[types.UID]struct{},
	newPlacements map[types.UID]string,
	maxTrials int, // <-- added
) bool {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	bestMoves, bestTarget, ok := bestPlanAcrossTargets(p, order, func(target *SolverNode) ([]MoveLite, bool) {
		// Direct fit: fast path
		if target.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
			return nil, true
		}
		var best []MoveLite
		bestCount := math.MaxInt32
		for trial := 0; trial < maxTrials; trial++ {
			mvs, ok := plan(p, target, nodes, order, moveGate, movedUIDs, trial, rng)
			if !ok {
				continue
			}
			if len(mvs) < bestCount {
				best, bestCount = mvs, len(mvs)
				if bestCount <= 1 {
					break
				}
			}
		}
		if best == nil {
			return nil, false
		}
		return best, true
	})
	if !ok {
		return false
	}
	return commitPlan(p, bestTarget, bestMoves, nodes, pods, order, newPlacements, movedUIDs)
}

func getVictims(target *SolverNode, opts VictimOptions) []*SolverPod {
	// filter by move gate / protected
	cands := make([]*SolverPod, 0, len(target.Pods))
	for _, q := range target.Pods {
		if !canMove(q, opts.MoveGate) {
			continue
		}
		cands = append(cands, q)
	}
	if len(cands) == 0 {
		return nil
	}

	switch opts.Strategy {
	case VictimsBFS:
		// Weighted coverage of the current deficit; keep BFS fanout small.
		total := max64(1, opts.NeedCPU) + max64(1, opts.NeedMem)
		wCPU := float64(max64(1, opts.NeedCPU)) / float64(total)
		wMem := 1.0 - wCPU
		sort.Slice(cands, func(i, j int) bool {
			vi, vj := cands[i], cands[j]
			scoreI := wCPU*float64(min64(vi.ReqCPUm, opts.NeedCPU)) +
				wMem*float64(min64(vi.ReqMemBytes, opts.NeedMem))
			scoreJ := wCPU*float64(min64(vj.ReqCPUm, opts.NeedCPU)) +
				wMem*float64(min64(vj.ReqMemBytes, opts.NeedMem))
			if scoreI != scoreJ {
				return scoreI > scoreJ
			}
			if vi.Priority != vj.Priority {
				return vi.Priority < vj.Priority
			}
			if vi.ReqCPUm != vj.ReqCPUm {
				return vi.ReqCPUm < vj.ReqCPUm
			}
			if vi.ReqMemBytes != vj.ReqMemBytes {
				return vi.ReqMemBytes < vj.ReqMemBytes
			}
			return vi.UID < vj.UID
		})

	case VictimsLocal:
		// Relocatability-aware multi-criteria ranking for local search.
		type scored struct {
			q           *SolverPod
			singleCover bool
			relocCount  int
			overshoot   int64
			alreadyMv   bool
		}
		sc := make([]scored, 0, len(cands))
		for _, q := range cands {
			_, already := opts.MovedUIDs[q.UID]
			single := (q.ReqCPUm >= opts.NeedCPU && q.ReqMemBytes >= opts.NeedMem)
			ov := max64(0, q.ReqCPUm-opts.NeedCPU) + max64(0, q.ReqMemBytes-opts.NeedMem)
			rc := 0
			if opts.Order != nil {
				for _, n := range opts.Order {
					if n.Name == target.Name {
						continue
					}
					if n.canPodFit(q.ReqCPUm, q.ReqMemBytes) {
						rc++
					}
				}
			}
			sc = append(sc, scored{q: q, singleCover: single, relocCount: rc, overshoot: ov, alreadyMv: already})
		}
		sort.Slice(sc, func(i, j int) bool {
			if sc[i].alreadyMv != sc[j].alreadyMv {
				return sc[i].alreadyMv
			}
			if sc[i].singleCover != sc[j].singleCover {
				return sc[i].singleCover
			}
			if sc[i].relocCount != sc[j].relocCount {
				return sc[i].relocCount > sc[j].relocCount
			}
			if sc[i].overshoot != sc[j].overshoot {
				return sc[i].overshoot < sc[j].overshoot
			}
			qi, qj := sc[i].q, sc[j].q
			if qi.Priority != qj.Priority {
				return qi.Priority < qj.Priority
			}
			si, sj := qi.ReqCPUm*qi.ReqMemBytes, qj.ReqCPUm*qj.ReqMemBytes
			if si != sj {
				return si > sj
			}
			if qi.ReqCPUm != qj.ReqCPUm {
				return qi.ReqCPUm < qj.ReqCPUm
			}
			if qi.ReqMemBytes != qj.ReqMemBytes {
				return qi.ReqMemBytes < qj.ReqMemBytes
			}
			return qi.UID < qj.UID
		})
		// rebuild ordered pods
		out := make([]*SolverPod, 0, len(sc))
		for _, s := range sc {
			out = append(out, s.q)
		}
		cands = out
	}

	// Randomize victim order a bit to get different plans on different trials.
	if opts.Rng != nil && opts.RandomizePct > 0 && len(cands) > 1 {
		p := float64(opts.RandomizePct) / 100.0
		if opts.Rng.Float64() < p {
			i := opts.Rng.Intn(len(cands))
			j := opts.Rng.Intn(len(cands))
			if i != j {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}

	if opts.Cap > 0 && opts.Cap < len(cands) {
		return cands[:opts.Cap]
	}

	return cands
}

// commitPlan verifies & applies `moves`, records them in newPlacements/movedUIDs,
// then places p on `target` (if it fits). If not, it falls back to bestDirectFit.
// Returns true on success, false if the plan is invalid or placement fails.
func commitPlan(
	p *SolverPod,
	target string,
	moves []MoveLite,
	nodes map[string]*SolverNode,
	pods map[types.UID]*SolverPod,
	order []*SolverNode,
	newPlacements map[types.UID]string,
	movedUIDs map[types.UID]struct{},
) bool {
	if !verifyPlan(nodes, pods, moves) {
		return false
	}
	for _, mv := range moves {
		newPlacements[mv.UID] = mv.To
		movedUIDs[mv.UID] = struct{}{}
	}
	if n := nodes[target]; n != nil && n.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
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

// placeOnePodCommon tries to place pod p using the given plan function.
// It returns (feasible, triedEvicting).
// If feasible is true, p was placed.
// If feasible is false and triedEvicting is true, it means an eviction was attempted but failed to place p.
// If feasible is false and triedEvicting is false, it means no eviction was attempted (e.g. cluster full).
func placeOnePodCommon(
	plan PlanFunc,
	p *SolverPod,
	nodes map[string]*SolverNode,
	pods map[types.UID]*SolverPod,
	order []*SolverNode,
	moveGate *int32,
	evictGate *int32,
	movedUIDs map[types.UID]struct{},
	newPlacements map[types.UID]string,
	evicts *[]Placement,
	maxTrials int,
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

	// 3) Relocations via provided plan
	if relocateViaPlan(plan, p, nodes, pods, order, moveGate, movedUIDs, newPlacements, maxTrials) {
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

	if on.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
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
//   - map of node name → *NodeType
//   - map of pod UID → *PodType
//   - slice of pending pods (to be scheduled)
//   - slice of all nodes in lexicographical order by name
//   - the preemptor pod if any (nil otherwise)
func buildClusterState(in SolverInput) (map[string]*SolverNode, map[types.UID]*SolverPod, []*SolverPod, []*SolverNode, *SolverPod) {
	// Nodes map + ordered slice
	nodes := make(map[string]*SolverNode, len(in.Nodes))
	order := make([]*SolverNode, 0, len(in.Nodes))
	for i := range in.Nodes {
		n := &SolverNode{
			Name:          in.Nodes[i].Name,
			CapCPUm:       in.Nodes[i].CapCPUm,
			CapMemBytes:   in.Nodes[i].CapMemBytes,
			Labels:        in.Nodes[i].Labels,
			AllocCPUm:     in.Nodes[i].CapCPUm,
			AllocMemBytes: in.Nodes[i].CapMemBytes,
			Pods:          make(map[types.UID]*SolverPod, 32),
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })

	// Pods map + pending list (+ preemptor ptr if any)
	pods := make(map[types.UID]*SolverPod, len(in.Pods)+1)
	pending := make([]*SolverPod, 0, len(in.Pods))
	var pre *SolverPod

	if in.Preemptor != nil {
		pi := *in.Preemptor // copy
		pi.Node = ""        // ensure pending
		pending = append(pending, &pi)
		pre = &pi
		pods[pi.UID] = pre
	}

	for i := range in.Pods {
		sp := in.Pods[i]
		p := &SolverPod{
			UID:         sp.UID,
			Namespace:   sp.Namespace,
			Name:        sp.Name,
			ReqCPUm:     sp.ReqCPUm,
			ReqMemBytes: sp.ReqMemBytes,
			Priority:    sp.Priority,
			Protected:   sp.Protected,
			Node:        sp.Node, // "where" in JSON
		}
		if p.Node == "" {
			pending = append(pending, p)
		}
		pods[p.UID] = p
		if p.Node != "" {
			if n := nodes[p.Node]; n != nil {
				n.addPod(p)
			}
		}
	}

	return nodes, pods, pending, order, pre
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
func stableOutput(status string, placements map[types.UID]string, evicts []Placement, in SolverInput) *SolverOutput {
	uids := make([]types.UID, 0, len(placements))
	for uid := range placements {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	lookup := func(uid types.UID) Pod {
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
func pickLargestEnablingEviction(order []*SolverNode, p *SolverPod, evictGate *int32, movedUIDs map[types.UID]struct{}) (*SolverPod, *SolverNode) {
	type cand struct {
		v  *SolverPod
		on *SolverNode
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
			if n.AllocCPUm+q.ReqCPUm >= p.ReqCPUm && n.AllocMemBytes+q.ReqMemBytes >= p.ReqMemBytes {
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

		si, sj := vi.ReqCPUm*vi.ReqMemBytes, vj.ReqCPUm*vj.ReqMemBytes
		if si != sj {
			return si > sj // larger first
		}
		if vi.ReqCPUm != vj.ReqCPUm {
			return vi.ReqCPUm > vj.ReqCPUm
		}
		if vi.ReqMemBytes != vj.ReqMemBytes {
			return vi.ReqMemBytes > vj.ReqMemBytes
		}
		if cands[i].on.Name != cands[j].on.Name {
			return cands[i].on.Name < cands[j].on.Name
		}
		return vi.UID < vj.UID
	})

	return cands[0].v, cands[0].on
}

// hasKey reports whether map m has key k.
func hasKey(m map[types.UID]struct{}, k types.UID) bool { _, ok := m[k]; return ok }

// buildOrigPlacements builds a map of pod UID → original node name from the current cluster state.
func buildOrigPlacements(order []*SolverNode) map[types.UID]string {
	orig := make(map[types.UID]string, 256)
	for _, n := range order {
		for _, q := range n.Pods {
			if q.Node != "" {
				orig[q.UID] = q.Node
			}
		}
	}
	return orig
}

// bestDirectFit finds the best-fit node for pod p in order.
// It returns the node name and true if found, or "", false if not found.
// Best-fit is defined as the node that minimizes CPU waste, then MEM waste,
// then lexicographically by node name.
func bestDirectFit(order []*SolverNode, p *SolverPod) (string, bool) {
	bestNode := ""
	bestCPUWaste := int64(math.MaxInt64)
	bestMEMWaste := int64(math.MaxInt64)
	for _, n := range order {
		if n.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
			cw := n.AllocCPUm - p.ReqCPUm
			mw := n.AllocMemBytes - p.ReqMemBytes
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
func clusterHasSlack(order []*SolverNode, p *SolverPod) bool {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.AllocCPUm
		mem += n.AllocMemBytes
	}
	return cpu >= p.ReqCPUm && mem >= p.ReqMemBytes
}

// evictGateForPod returns the eviction gate for pod p.
// In single-preemptor mode, the gate is the preemptor’s priority;
// in batch mode, it’s p.Priority.
func evictGateForPod(p *SolverPod, single bool, pre *SolverPod) *int32 {
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
func buildWorklist(pending []*SolverPod, pre *SolverPod) (out []*SolverPod, single bool, moveGate *int32) {
	if pre != nil {
		for _, p := range pending {
			if p.UID == pre.UID {
				mg := pre.Priority
				return []*SolverPod{p}, true, &mg
			}
		}
	}
	out = append(out, pending...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		sa, sb := a.ReqCPUm*a.ReqMemBytes, b.ReqCPUm*b.ReqMemBytes
		if sa != sb {
			return sa > sb
		}
		if a.ReqCPUm != b.ReqCPUm {
			return a.ReqCPUm > b.ReqCPUm
		}
		if a.ReqMemBytes != b.ReqMemBytes {
			return a.ReqMemBytes > b.ReqMemBytes
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
func verifyPlan(nodes map[string]*SolverNode, all map[types.UID]*SolverPod, moves []MoveLite) bool {
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
		df.cpu += p.ReqCPUm
		df.mem += p.ReqMemBytes
		per[src.Name] = df

		dt := per[dst.Name]
		dt.cpu -= p.ReqCPUm
		dt.mem -= p.ReqMemBytes
		per[dst.Name] = dt
	}

	for name, dd := range per {
		n := nodes[name]
		if n.AllocCPUm+dd.cpu < 0 || n.AllocMemBytes+dd.mem < 0 {
			klog.InfoS("apply: reject, final negative free",
				"node", name, "freeCPU_now", n.AllocCPUm, "freeMem_now", n.AllocMemBytes,
				"deltaCPU", dd.cpu, "deltaMem", dd.mem,
				"finalCPU", n.AllocCPUm+dd.cpu, "finalMem", n.AllocMemBytes+dd.mem)
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
		if !n.canPodFit(p.ReqCPUm, p.ReqMemBytes) {
			klog.InfoS("apply: unexpected no-fit at destination", "uid", p.UID, "to", n.Name)
			return false
		}
		n.addPod(p)
	}

	return true
}

// canPodFit returns true iff the node has enough free resources to fit the given cpu/mem request.
func (n *SolverNode) canPodFit(cpu, mem int64) bool {
	return n.AllocCPUm >= cpu && n.AllocMemBytes >= mem
}

// addPod adds pod p to node n, updating free resources accordingly.
func (n *SolverNode) addPod(p *SolverPod) {
	n.AllocCPUm -= p.ReqCPUm
	n.AllocMemBytes -= p.ReqMemBytes
	if n.Pods == nil {
		n.Pods = make(map[types.UID]*SolverPod, 16)
	}
	n.Pods[p.UID] = p
	p.Node = n.Name
}

// removePod removes pod p from node n, updating free resources accordingly.
func (n *SolverNode) removePod(p *SolverPod) {
	if _, ok := n.Pods[p.UID]; ok {
		delete(n.Pods, p.UID)
		n.AllocCPUm += p.ReqCPUm
		n.AllocMemBytes += p.ReqMemBytes
		p.Node = ""
	}
}

// computeSolverScore computes final Score from the snapshot given to the solver:
//   - placed_by_priority: number of pods that were placed for each priority
//   - evicted:            number of pods that were evicted
//   - moved:              number of pods that were moved to a different node
func computeSolverScore(in SolverInput, out *SolverOutput) SolverScore {
	if out == nil {
		return SolverScore{}
	}

	// Before-state (where) and priority by UID
	origWhere := make(map[types.UID]string, len(in.Pods)+1)
	pri := make(map[types.UID]int32, len(in.Pods)+1)
	for _, sp := range in.Pods {
		origWhere[sp.UID] = sp.Node
		pri[sp.UID] = sp.Priority
	}
	if in.Preemptor != nil {
		if _, ok := origWhere[in.Preemptor.UID]; !ok {
			origWhere[in.Preemptor.UID] = "" // pending
			pri[in.Preemptor.UID] = in.Preemptor.Priority
		}
	}

	// After-state starts from orig, then apply placements for known UIDs
	afterWhere := make(map[types.UID]string, len(origWhere))
	for uid, w := range origWhere {
		afterWhere[uid] = w
	}
	for _, plm := range out.Placements {
		if plm.ToNode == "" {
			continue
		}
		if _, known := afterWhere[plm.Pod.UID]; known {
			afterWhere[plm.Pod.UID] = plm.ToNode
		}
	}

	// Evicted
	evicted := make(map[types.UID]struct{}, len(out.Evictions))
	for _, e := range out.Evictions {
		evicted[e.Pod.UID] = struct{}{}
	}

	// Placed by priority
	placedByPri := map[string]int{}
	for uid, after := range afterWhere {
		if _, gone := evicted[uid]; gone {
			continue
		}
		if after != "" {
			key := strconv.Itoa(int(pri[uid]))
			placedByPri[key] = placedByPri[key] + 1
		}
	}

	// Moves
	moves := 0
	for uid, before := range origWhere {
		if _, gone := evicted[uid]; gone {
			continue
		}
		after := afterWhere[uid]
		if before != "" && after != "" && before != after {
			moves++
		}
	}

	return SolverScore{
		PlacedByPriority: placedByPri,
		Evicted:          len(evicted),
		Moved:            moves,
	}
}

// toSolverPod converts a Pod to a SolverPod.
func toSolverPod(p *v1.Pod, node string) SolverPod {
	return SolverPod{
		UID:         p.UID,
		Namespace:   p.Namespace,
		Name:        p.Name,
		ReqCPUm:     getPodCPURequest(p),
		ReqMemBytes: getPodMemoryRequest(p),
		Priority:    getPodPriority(p),
		Node:        node,
	}
}

// PreparedState holds the prepared cluster state for solvers.
func buildState(in SolverInput) *PreparedState {
	nodes, pods, pending, order, pre := buildClusterState(in)
	wl, single, mg := buildWorklist(pending, pre)
	return &PreparedState{
		Nodes:     nodes,
		Pods:      pods,
		Order:     order,
		Preemptor: pre,
		Worklist:  wl,
		Single:    single,
		MoveGate:  mg,
	}
}

// IsSolverEnabled checks if any solver is enabled.
func (pl *MyCrossNodePreemption) IsSolverEnabled() bool {
	return SolverPythonEnabled || SolverBfsEnabled || SolverLocalSearchEnabled
}

// HasSolverFeasibleResult checks if the solver output is feasible.
// OPTIMAL means the solution is perfect and meets all constraints (note there can be multiple optimal solutions and that the solver is non-deterministic).
// FEASIBLE means the solution is not perfect but still meets all constraints.
func HasSolverFeasibleResult(status string) bool {
	return status != "" && (status == "OPTIMAL" || status == "FEASIBLE")
}

// fillNodesAndPods adds nodes/pods using SharedInformerFactory listers.
// If batched != nil, pending batched pods are appended with where="" (and preemptor can be nil).
func (pl *MyCrossNodePreemption) fillNodesAndPods(
	in *SolverInput,
	nodes []*v1.Node,
	pods []*v1.Pod,
	preemptor *v1.Pod,
	batched []*v1.Pod,
	includePending bool,
) error {
	// Nodes
	usable := map[string]bool{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name:        n.Name,
			CapCPUm:     n.Status.Allocatable.Cpu().MilliValue(),
			CapMemBytes: n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}
	// Pods
	preUID := ""
	if preemptor != nil {
		preUID = string(preemptor.UID)
	}
	seen := make(map[types.UID]bool, len(pods)+len(batched))
	for _, p := range pods {
		// Skip the preemptor (it is provided via in.Preemptor)
		if string(p.UID) == preUID {
			continue
		}
		where := p.Spec.NodeName
		if where == "" {
			// Only include pending pods when explicitly asked (cohort mode).
			if !includePending {
				continue
			}
		} else {
			// If the pod is already bound to a node, ensure that node is usable.
			if !usable[where] {
				continue
			}
		}
		sp := toSolverPod(p, where)
		if p.Namespace == "kube-system" {
			sp.Protected = true
		}
		if !seen[sp.UID] {
			in.Pods = append(in.Pods, sp)
			seen[sp.UID] = true
		}
	}
	// Cohort: append the batched pending pods (where = ""), if requested
	if includePending {
		for _, p := range batched {
			if p == nil {
				continue
			}
			if string(p.UID) == preUID {
				continue
			}
			sp := toSolverPod(p, "")
			if p.Namespace == "kube-system" {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}
	}
	return nil
}

// buildSolverInput builds the common input for either batch or single.
func (pl *MyCrossNodePreemption) buildSolverInput(mode SolveMode, nodes []*v1.Node, pods []*v1.Pod, preemptor *v1.Pod, batched []*v1.Pod) (SolverInput, error) {
	in := SolverInput{
		IgnoreAffinity: true,
		LogProgress:    SolverLogProgress,
		Nodes:          make([]SolverNode, 0),
		Pods:           make([]SolverPod, 0),
		TimeoutMs:      0, // will be filled outside
	}
	switch mode {
	case SolveSingle:
		if preemptor == nil {
			return SolverInput{}, fmt.Errorf("SolveSingle requires preemptor")
		}
		pre := toSolverPod(preemptor, "")
		in.Preemptor = &pre
		if err := pl.fillNodesAndPods(&in, nodes, pods, preemptor, nil, false); err != nil {
			return SolverInput{}, fmt.Errorf("fill (single): %w", err)
		}
	case SolveBatch:
		if err := pl.fillNodesAndPods(&in, nodes, pods, nil, batched, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (batch): %w", err)
		}
	case SolveContinuous:
		if err := pl.fillNodesAndPods(&in, nodes, pods, nil, nil, true); err != nil {
			return SolverInput{}, fmt.Errorf("fill (continuous): %w", err)
		}
	default:
		return SolverInput{}, fmt.Errorf("unknown solve mode")
	}
	if len(in.Nodes) == 0 {
		return SolverInput{}, fmt.Errorf("no usable Ready nodes available; waiting")
	}
	return in, nil
}

// comparePlaced returns 1 if a>b, -1 if a<b, 0 if equal (lexi by priority desc).
func comparePlaced(a, b map[string]int) int {
	keys := map[int]struct{}{}
	for k := range a {
		if v, err := strconv.Atoi(k); err == nil {
			keys[v] = struct{}{}
		}
	}
	for k := range b {
		if v, err := strconv.Atoi(k); err == nil {
			keys[v] = struct{}{}
		}
	}
	prs := make([]int, 0, len(keys))
	for k := range keys {
		prs = append(prs, k)
	}
	// sort priorities descending
	sort.Sort(sort.Reverse(sort.IntSlice(prs)))
	for _, pr := range prs {
		ai := a[strconv.Itoa(pr)]
		bi := b[strconv.Itoa(pr)]
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

// cmpInt returns +1 if a<b (improvement because smaller is better),
// -1 if a>b (worse), 0 if equal.
func cmpInt(suggested, baseline int) int {
	switch {
	case suggested < baseline:
		return 1
	case suggested > baseline:
		return -1
	default:
		return 0
	}
}

// IsImprovement compares two scores lexicographically:
// 1) More placed per priority (lexicographic map compare)
// 2) Fewer evictions
// 3) Fewer moves
// Returns 1 if suggested is better, -1 if worse, 0 if equal.
func IsImprovement(baseline, suggested SolverScore) int {
	// 1) Placed-by-priority (more is better)
	if cmp := comparePlaced(suggested.PlacedByPriority, baseline.PlacedByPriority); cmp != 0 {
		klog.V(MyV).InfoS("Compare placed-by-priority", "result", cmp,
			"suggested", suggested.PlacedByPriority, "baseline", baseline.PlacedByPriority)
		return cmp
	}
	// 2) Evictions (fewer is better)
	if cmp := cmpInt(suggested.Evicted, baseline.Evicted); cmp != 0 {
		klog.V(MyV).InfoS("Compare evictions", "result", cmp,
			"suggested", suggested.Evicted, "baseline", baseline.Evicted)
		return cmp
	}
	// 3) Moves (fewer is better)
	if cmp := cmpInt(suggested.Moved, baseline.Moved); cmp != 0 {
		klog.V(MyV).InfoS("Compare moves", "result", cmp,
			"suggested", suggested.Moved, "baseline", baseline.Moved)
		return cmp
	}
	// Equal on all metrics
	klog.V(MyV).InfoS("No change: equal on placed, evictions, and moves")
	return 0
}

// planApplicable checks whether a SolverOutput (plan) can still be safely
// applied on the *current* cluster state. It allows unrelated drift and only
// insists that the concrete preconditions for the plan still hold.
func (pl *MyCrossNodePreemption) planApplicable(
	out *SolverOutput,
	nodes []*v1.Node,
	livePods []*v1.Pod,
) (bool, string) {
	if out == nil {
		return false, "nil plan"
	}

	// Index live state
	usable := map[string]bool{}
	capCPU := map[string]int64{}
	capMem := map[string]int64{}
	for _, n := range nodes {
		if isNodeUsable(n) {
			usable[n.Name] = true
			capCPU[n.Name] = n.Status.Allocatable.Cpu().MilliValue()
			capMem[n.Name] = n.Status.Allocatable.Memory().Value()
		}
	}

	type res struct{ cpu, mem int64 }
	used := map[string]res{} // by node
	pByUID := podsByUID(livePods)

	addUse := func(node string, cpu, mem int64) {
		u := used[node]
		u.cpu += cpu
		u.mem += mem
		used[node] = u
	}

	// Tally current usage
	for _, p := range livePods {
		if p == nil || p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
			continue
		}
		addUse(p.Spec.NodeName, getPodCPURequest(p), getPodMemoryRequest(p))
	}

	// Simulate the plan on top of current usage:
	// - Evictions free resources on their current node
	for _, e := range out.Evictions {
		p := pByUID[e.Pod.UID]
		if p == nil || p.Spec.NodeName == "" {
			// Already gone or pending now: keep going.
			continue
		}
		// If a node became unusable, fail.
		if !usable[p.Spec.NodeName] {
			return false, fmt.Sprintf("evict node now unusable: %s", p.Spec.NodeName)
		}
		addUse(p.Spec.NodeName, -getPodCPURequest(p), -getPodMemoryRequest(p))
	}

	// - Moves/placements: check pod still where we expect (pending or src), then add to dst
	for _, np := range out.Placements {
		p := pByUID[np.Pod.UID]
		if p == nil || p.DeletionTimestamp != nil {
			return false, fmt.Sprintf("pod vanished: %s", combineNsName(np.Pod.Namespace, np.Pod.Name))
		}
		// Source must still be consistent enough:
		//   - if it was a move (FromNode != ""), pod should still be on that source
		//   - if it was a new/pending placement (FromNode == ""), pod should still be pending
		if np.FromNode != "" {
			if p.Spec.NodeName != np.FromNode {
				return false, fmt.Sprintf("move precondition changed for %s/%s: was on %q, now on %q",
					p.Namespace, p.Name, np.FromNode, p.Spec.NodeName)
			}
			// remove from src (already accounted by evictions? No — moves are distinct)
			addUse(np.FromNode, -getPodCPURequest(p), -getPodMemoryRequest(p))
		} else {
			// pending expected
			if p.Spec.NodeName != "" {
				return false, fmt.Sprintf("pending precondition changed for %s/%s: now bound to %q",
					p.Namespace, p.Name, p.Spec.NodeName)
			}
		}

		// Destination must still be usable & have capacity
		if !usable[np.ToNode] {
			return false, fmt.Sprintf("dest node now unusable: %s", np.ToNode)
		}
		addUse(np.ToNode, getPodCPURequest(p), getPodMemoryRequest(p))
	}

	// Check that every node remains within capacity
	for node, u := range used {
		if u.cpu > capCPU[node] || u.mem > capMem[node] {
			return false, fmt.Sprintf("capacity exceeded on %s after sim: usedCPU=%d capCPU=%d usedMem=%d capMem=%d",
				node, u.cpu, capCPU[node], u.mem, capMem[node])
		}
	}
	return true, ""
}

// buildBaselineScore computes the baseline score from the solver input.
func buildBaselineScore(in SolverInput) SolverScore {
	placedByPri := map[string]int{}
	for _, sp := range in.Pods {
		if sp.Node == "" {
			continue // pending doesn't count into "placed"
		}
		pr := strconv.Itoa(int(sp.Priority))
		placedByPri[pr] = placedByPri[pr] + 1
	}
	return SolverScore{
		PlacedByPriority: placedByPri,
		Evicted:          0,
		Moved:            0,
	}
}

// freshClone returns deep-ish cloned nodes/pods/order and re-materializes
// the worklist against the cloned pods, so solvers can mutate safely.
func (ps *PreparedState) freshClone() (
	nodes map[string]*SolverNode,
	pods map[types.UID]*SolverPod,
	order []*SolverNode,
	worklist []*SolverPod,
) {
	// 1) clone pods
	pods = make(map[types.UID]*SolverPod, len(ps.Pods))
	for uid, p0 := range ps.Pods {
		cp := *p0
		pods[uid] = &cp
	}
	// 2) clone nodes (+ wire cloned pods into cloned nodes)
	nodes = make(map[string]*SolverNode, len(ps.Nodes))
	order = make([]*SolverNode, 0, len(ps.Order))
	for _, n0 := range ps.Order {
		n := &SolverNode{
			Name:          n0.Name,
			CapCPUm:       n0.CapCPUm,
			CapMemBytes:   n0.CapMemBytes,
			Labels:        n0.Labels,
			AllocCPUm:     n0.AllocCPUm,
			AllocMemBytes: n0.AllocMemBytes,
			Pods:          make(map[types.UID]*SolverPod, len(n0.Pods)),
		}
		for uid := range n0.Pods {
			if p := pods[uid]; p != nil {
				n.Pods[uid] = p
				p.Node = n.Name // reflect current location
			}
		}
		nodes[n.Name] = n
		order = append(order, n)
	}
	// 3) project worklist to cloned pods by UID
	worklist = make([]*SolverPod, len(ps.Worklist))
	for i, p0 := range ps.Worklist {
		worklist[i] = pods[p0.UID]
	}
	return
}

// helper near the top of run_solvers.go (or anywhere shared)
func cloneScore(s SolverScore) *SolverScore {
	var m map[string]int
	if s.PlacedByPriority != nil {
		m = make(map[string]int, len(s.PlacedByPriority))
		for k, v := range s.PlacedByPriority {
			m[k] = v
		}
	}
	return &SolverScore{
		PlacedByPriority: m,
		Evicted:          s.Evicted,
		Moved:            s.Moved,
	}
}
