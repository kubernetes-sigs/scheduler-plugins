// solver_fast.go
// Fast, greedy, depth-limited multi-hop move solver with a fixed move budget, tuned to avoid evictions.
// Per pending pod, repeat until placed or no more options:
//   1) Try direct fit.
//   2) If cluster has enough total free for the pod, try multi-hop move chains up to:
//        - SOLVER_FAST_MAX_CHAIN_DEPTH recursion depth
//        - SOLVER_FAST_MOVE_BUDGET total moves per attempt
//        - SOLVER_FAST_MOVE_ATTEMPTS different exploration orders (diversifies search before giving up)
//        Heuristics:
//          • Prefer targets with smallest deficit (CPU first, then MEM), but diversify attempts.
//          • Prefer moving victims that best cover the remaining deficit (CPU then MEM), then lowest prio, then smallest.
//          • Prefer destinations with most free resources; recursively free them within remaining depth/budget.
//   3) If all move attempts fail OR cluster total free is insufficient, evict exactly one lowest-priority non-protected pod
//      (strictly lower than the pending), then go back to (1).
// Evictions are minimized first (one-at-a-time), and we fully expend the move attempts/budget before evicting
// when the cluster has enough aggregate free resources (defragmentation).
//
// Batch/continuous modes:
//   - Sort pending pods by priority desc and apply the same loop per pod, carrying forward simulated state.
//
// Tuning via env:
//   - SOLVER_FAST_MAX_EVICTIONS   (default -1): -1 means unlimited; any non-negative caps total evictions per solver run.
//   - SOLVER_FAST_MAX_CHAIN_DEPTH (default 5) : maximum recursion depth for freeing nodes (multi-hop depth).
//   - SOLVER_FAST_MOVE_BUDGET     (default 6) : maximum number of moves allowed in a single chain attempt.
//   - SOLVER_FAST_MOVE_ATTEMPTS   (default 3) : number of diversified chain attempts before evicting.
//
// Expects these types elsewhere in the package: SolverInput, SolverOutput, SolverNode, SolverPod, SolverEviction, Score.

package mycrossnodepreemption

import (
	"math"
	"sort"
	"strconv"
)

// Env knobs
var (
	// -1 means unlimited evictions (no maximum).
	fastMaxEvictions = intFromEnvAllowNeg("SOLVER_FAST_MAX_EVICTIONS", -1)

	// Recursion depth & move budget
	fastMaxChainDepth = intFromEnv("SOLVER_FAST_MAX_CHAIN_DEPTH", 3)
	fastMoveBudget    = intFromEnv("SOLVER_FAST_MOVE_BUDGET", 5)

	// Diversified attempts before we consider eviction
	// (100 is very expensive; 4–8 gives most of the benefit)
	fastMoveAttempts = intFromEnv("SOLVER_FAST_MOVE_ATTEMPTS", 6)

	// NEW: prune search width per DFS frame
	fastVictimsPerLevel = intFromEnvAllowNeg("SOLVER_FAST_VICTIMS_PER_LEVEL", -1)
	fastDestsPerLevel   = intFromEnvAllowNeg("SOLVER_FAST_DESTS_PER_LEVEL", -1)
)

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func intFromEnv(key string, def int) int {
	v := getenv(key, "")
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

// Allows negative values (e.g., -1 for "unlimited").
func intFromEnvAllowNeg(key string, def int) int {
	v := getenv(key, "")
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

// --- Internal state structs ---

type podState struct {
	UID       string
	CPUm      int64
	MemBytes  int64
	Priority  int32
	Protected bool

	// current placement ("" means pending)
	Node string
}

type nodeState struct {
	Name     string
	CapCPUm  int64
	CapMem   int64
	FreeCPUm int64
	FreeMem  int64

	// resident pods by UID
	Pods map[string]*podState
}

func (n *nodeState) fits(cpu, mem int64) bool {
	return n.FreeCPUm >= cpu && n.FreeMem >= mem
}

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

type moveAction struct {
	UID  string
	From string
	To   string
}

// --- Core solver ---

func runFastSolver(in SolverInput) *SolverOutput {
	// Build initial state from input
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

	allPods := make(map[string]*podState, len(in.Pods)+1)
	var pendingList []*podState

	// Include preemptor (if any)
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

	// Other pods
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
		} else {
			if n := nodes[p.Node]; n != nil {
				n.addPod(p)
			}
		}
	}

	// Sort pending by priority desc, then by CPU desc, then Mem desc
	sort.Slice(pendingList, func(i, j int) bool {
		if pendingList[i].Priority != pendingList[j].Priority {
			return pendingList[i].Priority > pendingList[j].Priority
		}
		if pendingList[i].CPUm != pendingList[j].CPUm {
			return pendingList[i].CPUm > pendingList[j].CPUm
		}
		return pendingList[i].MemBytes > pendingList[j].MemBytes
	})

	// Collect final plan artifacts
	placements := make(map[string]string, len(pendingList))
	var evictedUIDs []SolverEviction

	// Track original placements for "moved" counting later
	originalNode := make(map[string]string, len(allPods))
	for uid, p := range allPods {
		originalNode[uid] = p.Node
	}

	// Eviction budget: -1 means unlimited
	evictionsLeft := fastMaxEvictions
	evictUnlimited := evictionsLeft < 0

	tryDirectFit := func(p *podState) (string, bool) {
		return selectDirectFit(nodes, nodeList, p)
	}

	// Main loop per pending pod: Direct fit -> diversified bounded move chains -> single eviction -> repeat
	placeOne := func(p *podState) bool {
		for {
			// 1) direct fit
			if nodeName, ok := tryDirectFit(p); ok {
				nodes[nodeName].addPod(p)
				placements[p.UID] = nodeName
				return true
			}

			// 2) defragment by moves only if cluster aggregate free can hold p
			totalCPU, totalMem := totalFreeResources(nodeList)
			if totalCPU >= p.CPUm && totalMem >= p.MemBytes {
				// Build ONE snapshot for all attempts (DFS backtracks to clean state)
				snap := newSnapshot(nodes, allPods)

				// Precompute target/destination orders once; reuse/rotate for diversification
				targetsDef := orderTargetsByDeficit(nodeList, p)
				targetsFree := orderByFreeDescFromSnapshot(snap)
				destOrder := orderByFreeDescFromSnapshot(snap)

				for attempt := 0; attempt < fastMoveAttempts; attempt++ {
					var targets []*nodeState
					switch attempt % 3 {
					case 0:
						targets = targetsDef
					case 1:
						targets = reverseNodesCopy(targetsDef)
					default:
						targets = targetsFree
					}
					if target, moves, ok := tryMoveChainToFitOnSnapshot(
						snap, p, targets, destOrder, fastMaxChainDepth, fastMoveBudget, attempt,
					); ok {
						applyMovesAndRecord(nodes, allPods, moves, placements)
						nodes[target].addPod(p)
						placements[p.UID] = target
						return true
					}
					// snapshot is clean thanks to DFS backtracking; no reset needed
				}
			}

			// 3) single eviction then loop
			if evictUnlimited || evictionsLeft > 0 {
				victim, vNode := pickSingleEvictionCandidate(nodeList, p)
				if victim == nil || vNode == nil {
					return false
				}
				delete(vNode.Pods, victim.UID)
				vNode.FreeCPUm += victim.CPUm
				vNode.FreeMem += victim.MemBytes
				evictedUIDs = append(evictedUIDs, SolverEviction{UID: victim.UID})
				if !evictUnlimited {
					evictionsLeft--
				}
				continue
			}
			return false
		}
	}

	for _, p := range pendingList {
		if placeOne(p) {
			// Single-preemptor: stop after placing it
			if in.Preemptor != nil && p.UID == in.Preemptor.UID {
				break
			}
		} else {
			// Single-preemptor: stop if it cannot be placed
			if in.Preemptor != nil && p.UID == in.Preemptor.UID {
				break
			}
		}
	}

	// Build Score
	score := buildScoreFromState(allPods, originalNode, evictedUIDs)

	out := &SolverOutput{
		Status:        "FEASIBLE",
		NominatedNode: "",
		Placements:    placements,
		Evictions:     evictedUIDs,
		Score:         score,
	}

	// Nomination for single-preemptor flows
	if in.Preemptor != nil {
		if dest, ok := placements[in.Preemptor.UID]; ok {
			out.NominatedNode = dest
		}
	}

	return out
}

// Replace dfsFreeForPod with this version
func dfsFreeForPodFast(
	snap *snapshot,
	target string,
	p *podState,
	prioLimit int,
	depth int,
	budget int,
	visited map[string]bool,
	plan *[]moveAction,
	attempt int,
	destOrder []*nodeState, // static preordered by free
) bool {
	if depth < 0 || budget <= 0 {
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

	// ---- compute K’s from env ( -1 => unlimited ) ----
	// victims K: unlimited => use budget (we can’t move more than budget anyway)
	vK := budget
	if fastVictimsPerLevel >= 0 {
		vK = min(budget, fastVictimsPerLevel)
	}
	// dests K: unlimited => pass 0 to topKDests (it returns the full rotated list when K<=0)
	dK := fastDestsPerLevel
	if dK >= 0 {
		dK = min(dK, len(destOrder))
	}

	// quick prune: can the best vK victims even cover deficits?
	if !canCoverDeficitTopK(t, prioLimit, needCPU, needMem, vK) {
		return false
	}

	// pick top vK victims and top dK destinations
	victims := topKVictims(t.Pods, prioLimit, needCPU, needMem, vK, visited, attempt)
	if len(victims) == 0 {
		return false
	}
	dests := topKDests(destOrder, dK, attempt)

	planStart := len(*plan)

	for _, v := range victims {
		if visited[v.UID] {
			continue
		}
		for _, dn := range dests {
			if dn.Name == target && !(t.FreeCPUm+v.CPUm >= p.CPUm && t.FreeMem+v.MemBytes >= p.MemBytes) {
				continue
			}
			used := len(*plan) - planStart
			rem := budget - used
			if rem <= 0 {
				return false
			}

			// Direct destination
			if snap.nodes[dn.Name].fits(v.CPUm, v.MemBytes) {
				if doMove(snap, v.UID, t.Name, dn.Name, plan) {
					if snap.nodes[target].fits(p.CPUm, p.MemBytes) {
						return true
					}
					if dfsFreeForPodFast(snap, target, p, prioLimit, depth, budget-(len(*plan)-planStart), visited, plan, attempt, destOrder) {
						return true
					}
					undoLastMove(snap, plan)
				}
				continue
			}

			// Free destination first (reserve 1 move for the victim)
			if rem <= 1 || depth-1 < 0 {
				continue
			}
			visited[v.UID] = true
			mark := len(*plan)
			if dfsFreeForPodFast(snap, dn.Name, v, prioLimit, depth-1, rem-1, visited, plan, attempt, destOrder) &&
				snap.nodes[dn.Name].fits(v.CPUm, v.MemBytes) &&
				doMove(snap, v.UID, t.Name, dn.Name, plan) {

				if snap.nodes[target].fits(p.CPUm, p.MemBytes) {
					visited[v.UID] = false
					return true
				}
				if dfsFreeForPodFast(snap, target, p, prioLimit, depth, budget-(len(*plan)-planStart), visited, plan, attempt, destOrder) {
					visited[v.UID] = false
					return true
				}
				undoLastMove(snap, plan)
			}
			// backtrack any frees
			for len(*plan) > mark {
				undoLastMove(snap, plan)
			}
			visited[v.UID] = false
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pick up to K best victims by deficit coverage, then lowest priority, then smaller size
// pick up to K best victims by deficit coverage, then lowest priority, then smaller size.
// K <= 0 means "unlimited" (consider all eligible).
func topKVictims(pods map[string]*podState, prioLimit int, needCPU, needMem int64, K int, visited map[string]bool, attempt int) []*podState {
	if K <= 0 {
		K = len(pods) // effectively unlimited
	}
	type vicSc struct {
		p                *podState
		cpuGain, memGain int64
		pri              int32
		score            int64 // prioritize coverage; tweakable
	}
	buf := make([]vicSc, 0, K)

	push := func(v *podState) {
		cpu := min64(v.CPUm, needCPU)
		mem := min64(v.MemBytes, needMem)
		sc := vicSc{p: v, cpuGain: cpu, memGain: mem, pri: v.Priority, score: cpu*3 + mem*2}
		if len(buf) < K {
			buf = append(buf, sc)
			return
		}
		// replace the worst if better
		w := 0
		for i := 1; i < len(buf); i++ {
			// "worse" if lower score, or higher priority, or larger
			if buf[i].score < buf[w].score ||
				(buf[i].score == buf[w].score && (buf[i].pri > buf[w].pri ||
					(buf[i].pri == buf[w].pri && (buf[i].p.CPUm > buf[w].p.CPUm ||
						(buf[i].p.CPUm == buf[w].p.CPUm && buf[i].p.MemBytes > buf[w].p.MemBytes))))) {
				w = i
			}
		}
		// better?
		if sc.score > buf[w].score ||
			(sc.score == buf[w].score && (sc.pri < buf[w].pri ||
				(sc.pri == buf[w].pri && (sc.p.CPUm < buf[w].p.CPUm ||
					(sc.p.CPUm == buf[w].p.CPUm && sc.p.MemBytes < buf[w].p.MemBytes))))) {
			buf[w] = sc
		}
	}

	for _, rp := range pods {
		if rp.Protected || int(rp.Priority) > prioLimit || visited[rp.UID] {
			continue
		}
		push(rp)
	}

	sort.Slice(buf, func(i, j int) bool {
		if buf[i].score != buf[j].score {
			return buf[i].score > buf[j].score
		}
		if buf[i].pri != buf[j].pri {
			return buf[i].pri < buf[j].pri
		}
		if buf[i].p.CPUm != buf[j].p.CPUm {
			return buf[i].p.CPUm < buf[j].p.CPUm
		}
		return buf[i].p.MemBytes < buf[j].p.MemBytes
	})

	out := make([]*podState, len(buf))
	for i := range buf {
		out[i] = buf[i].p
	}

	if attempt%2 == 1 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func topKDests(order []*nodeState, K, attempt int) []*nodeState {
	if K <= 0 || K >= len(order) {
		// rotate slightly for diversity
		if len(order) == 0 {
			return nil
		}
		rot := attempt % len(order)
		out := append(order[rot:], order[:rot]...)
		return out
	}
	out := make([]*nodeState, 0, K)
	// simple rotation
	rot := 0
	if len(order) > 0 {
		rot = attempt % len(order)
	}
	for i := 0; i < K; i++ {
		out = append(out, order[(rot+i)%len(order)])
	}
	return out
}

// If K <= 0: treat as unlimited (sum all eligible victims exactly).
func canCoverDeficitTopK(t *nodeState, prioLimit int, needCPU, needMem int64, K int) bool {
	if K <= 0 {
		var sumC, sumM int64
		for _, rp := range t.Pods {
			if rp.Protected || int(rp.Priority) > prioLimit {
				continue
			}
			sumC += rp.CPUm
			sumM += rp.MemBytes
		}
		return sumC >= needCPU && sumM >= needMem
	}

	// top-K by simple “keep K largest” sketch
	topCPU := make([]int64, 0, K)
	topMem := make([]int64, 0, K)
	add := func(arr *[]int64, val int64) {
		if len(*arr) < cap(*arr) {
			*arr = append(*arr, val)
			return
		}
		// replace smallest if bigger
		w := 0
		for i := 1; i < len(*arr); i++ {
			if (*arr)[i] < (*arr)[w] {
				w = i
			}
		}
		if val > (*arr)[w] {
			(*arr)[w] = val
		}
	}
	for _, rp := range t.Pods {
		if rp.Protected || int(rp.Priority) > prioLimit {
			continue
		}
		add(&topCPU, rp.CPUm)
		add(&topMem, rp.MemBytes)
	}
	var sumC, sumM int64
	for _, v := range topCPU {
		sumC += v
	}
	for _, v := range topMem {
		sumM += v
	}
	return sumC >= needCPU && sumM >= needMem
}

// same signature but takes prebuilt snapshot & preordered lists
func tryMoveChainToFitOnSnapshot(
	snap *snapshot,
	p *podState,
	targets []*nodeState, // preordered targets
	destOrder []*nodeState, // preordered dests
	maxDepth int,
	moveBudget int,
	attempt int,
) (string, []moveAction, bool) {
	visited := make(map[string]bool, 32)
	for _, t := range targets {
		if snap.nodes[t.Name].fits(p.CPUm, p.MemBytes) {
			continue
		}
		moves := make([]moveAction, 0, moveBudget)
		if dfsFreeForPodFast(snap, t.Name, p, int(p.Priority), maxDepth, moveBudget, visited, &moves, attempt, destOrder) {
			return t.Name, append([]moveAction(nil), moves...), true
		}
	}
	return "", nil, false
}

func reverseNodesCopy(a []*nodeState) []*nodeState {
	out := make([]*nodeState, len(a))
	copy(out, a)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
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

// totalFreeResources returns aggregate free CPU and Mem across nodes.
func totalFreeResources(order []*nodeState) (int64, int64) {
	var cpu, mem int64
	for _, n := range order {
		cpu += n.FreeCPUm
		mem += n.FreeMem
	}
	return cpu, mem
}

// --- Move-chain search (bounded depth and move budget, diversified attempts) ---

// snapshot is a lightweight in-memory state we mutate during search and backtrack.
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
	// Ensure source contains the pod
	if fn.Pods[uid] == nil {
		return false
	}
	// Ensure destination has capacity for this pod
	if !tn.fits(p.CPUm, p.MemBytes) {
		return false
	}
	// Apply move
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
	// reverse
	_ = snap.move(last.UID, last.To, last.From)
	*plan = (*plan)[:len(*plan)-1]
}

func orderByFreeDescFromSnapshot(snap *snapshot) []*nodeState {
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
	return ns
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// applyMovesAndRecord applies move actions to the real state and records placements for moved pods.
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

func selectDirectFit(nodes map[string]*nodeState, order []*nodeState, p *podState) (string, bool) {
	bestNode := ""
	bestWaste := int64(math.MaxInt64) // minimize leftover waste (CPU), then MEM
	for _, n := range order {
		if n.fits(p.CPUm, p.MemBytes) {
			waste := (n.FreeCPUm - p.CPUm)
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

// pickSingleEvictionCandidate chooses a single victim pod whose eviction lets the pod p fit on that node.
// It returns the lowest-priority eligible victim (strictly lower prio than p), preferring the smallest resource
// that still makes p fit. If multiple nodes qualify, returns the globally "cheapest" victim.
func pickSingleEvictionCandidate(order []*nodeState, p *podState) (*podState, *nodeState) {
	var bestVictim *podState
	var bestNode *nodeState
	var bestKey struct {
		priority int32
		cpu      int64
		mem      int64
	}
	bestKey.priority = math.MaxInt32
	bestKey.cpu = math.MaxInt64
	bestKey.mem = math.MaxInt64

	for _, n := range order {
		needCPU := p.CPUm - n.FreeCPUm
		needMem := p.MemBytes - n.FreeMem
		if needCPU <= 0 && needMem <= 0 {
			// Node already fits; no eviction needed
			continue
		}
		// Consider victims on this node sorted by priority asc (lowest first), then by CPU asc
		cands := make([]*podState, 0, len(n.Pods))
		for _, rp := range n.Pods {
			if rp.Protected {
				continue
			}
			// Do not evict higher/equal-priority than the pending pod
			if rp.Priority >= p.Priority {
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
			// Single victim must allow fit
			if n.FreeCPUm+v.CPUm >= p.CPUm && n.FreeMem+v.MemBytes >= p.MemBytes {
				key := struct {
					priority int32
					cpu      int64
					mem      int64
				}{v.Priority, v.CPUm, v.MemBytes}
				if key.priority < bestKey.priority ||
					(key.priority == bestKey.priority && (key.cpu < bestKey.cpu ||
						(key.cpu == bestKey.cpu && key.mem < bestKey.mem))) {
					bestVictim = v
					bestNode = n
					bestKey = key
				}
				break // on this node, we found the best victim
			}
		}
	}
	return bestVictim, bestNode
}

// --- Score computation ---

func buildScoreFromState(allPods map[string]*podState, originalNode map[string]string, evicted []SolverEviction) Score {
	// Compute placed counts by priority (post-plan)
	placedByPri := map[string]int{}
	placedUID := make(map[string]bool, len(allPods))

	// Mark evicted
	evictedSet := make(map[string]bool, len(evicted))
	for _, e := range evicted {
		evictedSet[e.UID] = true
	}

	// Count pods that remain placed (i.e., have Node != "" and not evicted)
	for _, p := range allPods {
		if evictedSet[p.UID] {
			continue
		}
		if p.Node != "" {
			pr := strconv.Itoa(int(p.Priority))
			placedByPri[pr] = placedByPri[pr] + 1
			placedUID[p.UID] = true
		}
	}

	// Moves: count number of pods that had a node and changed node (exclude pending pods newly placed)
	moves := 0
	for uid, p := range allPods {
		from := originalNode[uid]
		to := p.Node
		if from != "" && to != "" && from != to && !evictedSet[uid] {
			moves++
		}
	}

	return Score{
		PlacedByPriority: placedByPri,
		Evicted:          len(evicted),
		Moved:            moves,
	}
}
