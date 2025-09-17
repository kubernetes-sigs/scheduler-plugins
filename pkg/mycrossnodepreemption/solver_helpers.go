// solver_helpers.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// PlanFunc is a function that, given a pod and a target node, tries to find a plan.
type PlanFunc func(
	pending *SolverPod,
	target *SolverNode,
	nodes map[string]*SolverNode,
	order []*SolverNode,
	moveGate *int32,
	movedUIDs map[types.UID]struct{},
	trial int,
	rng *rand.Rand,
) ([]MoveLite, bool)

type MoveLite struct {
	UID  types.UID
	From string
	To   string
}

// TargetScore is used to order nodes by how well they can accommodate a pod.
type TargetScore struct {
	Node   *SolverNode
	Score  float64 // max(defCPU/p.CPU, defMEM/p.MEM)
	DefSum int64
	Waste  int64
}

type VictimStrategy int

const (
	VictimsBFS   VictimStrategy = iota // coverage-first for BFS
	VictimsLocal                       // relocatability-aware for local search
)

// VictimOptions holds options for getVictims.
type VictimOptions struct {
	Strategy     VictimStrategy
	MoveGate     *int32                 // priority gate for moves
	NeedCPU      int64                  // remaining CPU deficit on the active node
	NeedMem      int64                  // remaining MEM deficit on the active node
	Cap          int                    // max victims to return (0 = no cap)
	Order        []*SolverNode          // required for VictimsLocal (to compute relocCount)
	MovedUIDs    map[types.UID]struct{} // prefer already-moved in local
	Rng          *rand.Rand             // for randomization (nil = none)
	RandomizePct int                    // % of randomization of victim order (0 = none)
}

// Delta represents a change in CPU and Memory.
type Delta struct {
	CPU int64
	Mem int64
}

// UIDSet is a set of pod UIDs.
type UIDSet map[string]struct{}

// Add adds a UID to the set.
func (s UIDSet) Add(uid string) { s[uid] = struct{}{} }

// Delete removes a UID from the set.
func (s UIDSet) Delete(uid string) { delete(s, uid) }

// Has checks if a UID is in the set.
func (s UIDSet) Has(uid string) bool { _, ok := s[uid]; return ok }

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
	klog.V(V2).InfoS("Running solver", "tag", tag)
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

type SolverStats struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationUs int64  `json:"duration_us"`
	Score      Score  `json:"score"`
}

type ExportedStats struct {
	Timestamp_ns int64         `json:"timestamp_ns"`
	Baseline     Score         `json:"baseline"`
	Attempts     []SolverStats `json:"attempts"`
	Chosen       *SolverStats  `json:"chosen,omitempty"`
	PlanStatus   PlanStatus    `json:"plan_status,omitempty"`
}

const (
	cmExportedStatsName      = "stats"
	cmExportedStatsNamespace = "stats"
	cmExportedStatsKey       = "runs.json" // JSON array of solverRunEvent
)

// append (create if missing) an entry to the ConfigMap ledger
func (pl *MyCrossNodePreemption) appendStatsCM(ctx context.Context, entry ExportedStats) {
	cli := pl.Handle.ClientSet()
	if cli == nil {
		klog.V(1).Info("no clientset; skip stats CM")
		return
	}
	cms := cli.CoreV1().ConfigMaps(cmExportedStatsNamespace)
	cm, err := cms.Get(ctx, cmExportedStatsName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "get CM failed", "namespace", cmExportedStatsNamespace, "name", cmExportedStatsName)
			return
		}
		// create fresh
		buf, _ := json.Marshal([]ExportedStats{entry})
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmExportedStatsName,
				Namespace: cmExportedStatsNamespace,
			},
			Data: map[string]string{cmExportedStatsKey: string(buf)},
		}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			klog.ErrorS(err, "create CM failed", "namespace", cmExportedStatsNamespace, "name", cmExportedStatsName)
		}
		return
	}
	// update existing
	var arr []ExportedStats
	if s := cm.Data[cmExportedStatsKey]; s != "" {
		_ = json.Unmarshal([]byte(s), &arr)
		// best-effort
	}
	arr = append(arr, entry)
	buf, _ := json.Marshal(arr)
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[cmExportedStatsKey] = string(buf)
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "update CM failed", "namespace", cmExportedStatsNamespace, "name", cmExportedStatsName)
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
