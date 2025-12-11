// solver_helpers.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clientv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// isAnySolverEnabled checks if any solver is enabled.
func (pl *SharedState) isAnySolverEnabled() bool {
	return SolverPythonEnabled // add more using ORs as needed
}

// buildSolverInput builds the solver input from live nodes/pods (and optional preemptor)
func (pl *SharedState) buildSolverInput(
	nodes []*v1.Node,
	pods []*v1.Pod,
	preemptor *v1.Pod,
) (PlannerInput, error) {

	in := PlannerInput{
		IgnoreAffinity: true,
		Nodes:          make([]PlannerNode, 0, len(nodes)),
		Pods:           make([]PlannerPod, 0, len(pods)),
	}

	// ----- Nodes: keep only usable -----
	usable := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		in.Nodes = append(in.Nodes, PlannerNode{
			Name:        n.Name,
			CapCPUm:     n.Status.Allocatable.Cpu().MilliValue(),
			CapMemBytes: n.Status.Allocatable.Memory().Value(),
		})
		usable[n.Name] = true
	}
	if len(in.Nodes) == 0 {
		return PlannerInput{}, ErrNoUsableNodes
	}

	// ----- Preemptor: include as pending (not in Pods list) -----
	preUID := ""
	if preemptor != nil {
		pre := toPod(preemptor, "")
		in.Preemptor = &pre
		preUID = string(preemptor.UID)
	}

	// ----- Pods: add running-on-usable + all pending, skip preemptor -----
	seen := make(map[types.UID]bool, len(pods))
	for _, p := range pods {
		if p == nil {
			continue
		}
		if string(p.UID) == preUID {
			// preemptor is represented via in.Preemptor, never duplicated in Pods
			continue
		}

		where := getPodAssignedNodeName(p)
		switch {
		case where == "":
			// pending → always include
			sp := toPod(p, "")
			if isPodProtected(p) {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}

		default:
			// running -> include only if bound to a usable node
			if !usable[where] {
				continue
			}
			sp := toPod(p, where)
			if isPodProtected(p) {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}
	}

	in.BaselineScore = buildBaselineScore(pods)

	return in, nil
}

// buildBaselineScore computes the baseline score from the solver input.
func buildBaselineScore(pods []*v1.Pod) PlannerScore {
	placedByPri := map[string]int{}
	for _, p := range pods {
		if !isPodAssigned(p) {
			continue // pending doesn't count into "placed"
		}
		pr := strconv.Itoa(int(getPodPriority(p)))
		placedByPri[pr] = placedByPri[pr] + 1
	}
	return PlannerScore{
		PlacedByPriority: placedByPri,
		Evicted:          0,
		Moved:            0,
	}
}

// solverConfigArgs builds a list of key-value pairs representing the active solver configuration.
func solverConfigArgs() []any {
	args := make([]any, 0, 10)
	if SolverPythonEnabled {
		args = append(
			args,
			"pythonSolver", true,
			"pythonTimeout", SolverPythonTimeout.String(),
			"pythonGapLimit", fmt.Sprintf("%.2f", SolverPythonGapLimit),
			"pythonGuaranteedTierFraction", fmt.Sprintf("%.2f", SolverPythonGuaranteedTierFraction),
			"pythonMoveFractionOfTier", fmt.Sprintf("%.2f", SolverPythonMoveFractionOfTier),
		)
	}
	// Always include shared flags
	args = append(args, "saveFailedAttempts", SolverSaveAllAttempts)
	return args
}

// isImprovement compares two scores lexicographically:
//  1. More placed per priority (lexicographic map compare)
//  2. Fewer evictions
//  3. Fewer moves
//
// Returns 1 if suggested is better, -1 if worse, 0 if equal.
// Returns as soon as a difference is found.
func isImprovement(baseline, suggested *PlannerScore) int {
	// 1) Placed-by-priority (more is better)
	if cmp := comparePlaced(suggested.PlacedByPriority, baseline.PlacedByPriority); cmp != 0 {
		klog.V(MyV).InfoS("compare placed-by-priority", "result", cmp,
			"suggested", suggested.PlacedByPriority, "baseline", baseline.PlacedByPriority)
		return cmp
	}

	// 2) Evictions (fewer is better)
	if cmp := cmpInt(suggested.Evicted, baseline.Evicted); cmp != 0 {
		klog.V(MyV).InfoS("compare evictions", "result", cmp,
			"suggested", suggested.Evicted, "baseline", baseline.Evicted)
		return cmp
	}

	// 3) Moves (fewer is better)
	if cmp := cmpInt(suggested.Moved, baseline.Moved); cmp != 0 {
		klog.V(MyV).InfoS("compare moves", "result", cmp,
			"suggested", suggested.Moved, "baseline", baseline.Moved)
		return cmp
	}

	// Equal on all metrics
	klog.V(MyV).InfoS("no change: equal on placed, evictions, and moves")
	return 0
}

// comparePlaced returns 1 if a>b, -1 if a<b, 0 if equal (lexi by priority desc).
func comparePlaced(a, b map[string]int) int {
	keys := map[int]struct{}{}
	for key := range a {
		if value, err := strconv.Atoi(key); err == nil {
			keys[value] = struct{}{}
		}
	}
	for key := range b {
		if value, err := strconv.Atoi(key); err == nil {
			keys[value] = struct{}{}
		}
	}
	priorities := make([]int, 0, len(keys))
	for k := range keys {
		priorities = append(priorities, k)
	}
	// returns as soon as a difference is found starting from highest prio
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	for _, pr := range priorities {
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

// cmpInt returns +1 if suggested<baseline (improvement because smaller is better),
// -1 if suggested>baseline (worse), 0 if equal.
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

// hasSolverUsableResult checks if the solver output status indicates a usable result.
// OPTIMAL means the solution is perfect and meets all constraints
// (there can be multiple optimal solutions and the solver is non-deterministic).
// FEASIBLE means the solution is not optimal but still meets all constraints.
func hasSolverUsableResult(status string) bool {
	return status != "" && (status == "OPTIMAL" || status == "FEASIBLE")
}

// doesSolverSolutionImproves reports whether a solver result is both usable
// (status is OPTIMAL/FEASIBLE) and strictly better than the given baseline.
// Returns:
//
//	improves: true if usable AND strictly better than baseline
//	usable:   true if status indicates a usable solution (OPTIMAL/FEASIBLE)
func doesSolverSolutionImproves(
	baseline *PlannerScore,
	status string,
	score *PlannerScore,
) (improves bool, usable bool) {
	if baseline == nil || score == nil {
		return false, false
	}
	usable = hasSolverUsableResult(status)
	if !usable {
		return false, false
	}
	return isImprovement(baseline, score) > 0, true
}

// isPlanApplicable checks whether a SolverOutput can still be safely
// applied on the current cluster state. It allows unrelated drift and only
// insists that the concrete preconditions for the plan still hold.
func (pl *SharedState) isPlanApplicable(
	out *PlannerOutput,
	nodes []*v1.Node,
	pods []*v1.Pod,
) (bool, string) {
	if out == nil {
		return false, "nil plan"
	}

	// Index live, usable nodes and their capacities.
	usable := map[string]bool{}
	capCPU := map[string]int64{}
	capMem := map[string]int64{}
	for _, n := range nodes {
		if !isNodeUsable(n) {
			continue
		}
		usable[n.Name] = true
		capCPU[n.Name] = getNodeCPUAllocatable(n)
		capMem[n.Name] = getNodeMemoryAllocatable(n)
	}

	type res struct{ cpu, mem int64 }
	used := map[string]res{} // current usage per node
	pByUID := podsByUID(pods)

	addUse := func(node string, cpu, mem int64) {
		u := used[node]
		u.cpu += cpu
		u.mem += mem
		used[node] = u
	}

	// Tally current usage (only assigned, non-terminating pods).
	for _, p := range pods {
		if !isPodAssignedAndAlive(p) {
			continue
		}
		node := getPodAssignedNodeName(p)
		addUse(node, getPodCPURequest(p), getPodMemoryRequest(p))
	}

	// Apply evictions from the plan: free resources on their current node.
	for _, e := range out.Evictions {
		p := pByUID[e.UID]
		if !isPodAssignedAndAlive(p) {
			// Already gone or pending now: keep going.
			continue
		}
		node := getPodAssignedNodeName(p)
		if !usable[node] {
			return false, fmt.Sprintf("evict node now unusable: %s", node)
		}
		addUse(node, -getPodCPURequest(p), -getPodMemoryRequest(p))
	}

	// Apply moves/placements from the plan.
	for _, np := range out.Placements {
		p := pByUID[np.UID]
		if isPodDeleted(p) {
			return false, fmt.Sprintf("pod vanished: %s", mergeNsName(np.Namespace, np.Name))
		}

		currentNode := getPodAssignedNodeName(p)

		// Source must still be consistent:
		//   - move: pod should still be on OldNode
		//   - new placement: pod should still be pending
		if np.OldNode != "" {
			if currentNode != np.OldNode {
				return false, fmt.Sprintf(
					"move precondition changed for %s/%s: was on %q, now on %q",
					p.Namespace, p.Name, np.OldNode, currentNode,
				)
			}
			// Remove from source node (moves are distinct from evictions).
			addUse(np.OldNode, -getPodCPURequest(p), -getPodMemoryRequest(p))
		} else {
			// Expected to be pending.
			if currentNode != "" {
				return false, fmt.Sprintf(
					"pending precondition changed for %s/%s: now bound to %q",
					p.Namespace, p.Name, currentNode,
				)
			}
		}

		// Destination must still be usable & have capacity.
		if !usable[np.Node] {
			return false, fmt.Sprintf("dest node now unusable: %s", np.Node)
		}
		addUse(np.Node, getPodCPURequest(p), getPodMemoryRequest(p))
	}

	// Final capacity check per node.
	for node, u := range used {
		if u.cpu > capCPU[node] || u.mem > capMem[node] {
			return false, fmt.Sprintf(
				"capacity exceeded on %s after sim: usedCPU=%d capCPU=%d usedMem=%d capMem=%d",
				node, u.cpu, capCPU[node], u.mem, capMem[node],
			)
		}
	}

	return true, ""
}

// logLeaderboard prints a compact solver leaderboard relative to baseline.
// It groups attempts as better/equal/worse vs baseline and tags adjacent ties.
// If best is nil, it logs only the baseline row.
func logLeaderboard(
	label string,
	attempts []PlannerResult,
	baseline PlannerScore,
	best *PlannerResult,
) {
	// No best attempt → only log baseline (useful when all solvers failed / unusable).
	if best == nil {
		klog.InfoS(
			msg(label, "solver leaderboard"),
			"ranking", []string{"baseline"},
			"status", []string{"BASELINE"},
			"durationsUs", []int64{0},
			"evictions", []int{baseline.Evicted},
			"moves", []int{baseline.Moved},
			"placedByPri", baseline.PlacedByPriority,
			"prevPlacedByPri", baseline.PlacedByPriority,
		)
		return
	}

	if len(attempts) == 0 {
		attempts = nil
	}

	var better, equal, worse []PlannerResult
	for _, r := range attempts {
		cmp := isImprovement(&baseline, &r.Score)
		switch cmp {
		case 1:
			better = append(better, r)
		case 0:
			equal = append(equal, r)
		default:
			worse = append(worse, r)
		}
	}

	// Baseline entry – first among equals
	baselineEntry := PlannerResult{
		Name:       "baseline",
		Status:     "BASELINE",
		DurationMs: 0,
		Score:      baseline,
	}
	equal = append([]PlannerResult{baselineEntry}, equal...)

	ranking := append(append(better, equal...), worse...)

	tied := func(a, b PlannerScore) bool {
		return isImprovement(&a, &b) == 0 && isImprovement(&b, &a) == 0
	}

	names := make([]string, len(ranking))
	statuses := make([]string, len(ranking))
	evictions := make([]int, len(ranking))
	moves := make([]int, len(ranking))
	durations := make([]int64, len(ranking))
	for i := range ranking {
		lbl := ranking[i].Name
		if i > 0 && tied(ranking[i-1].Score, ranking[i].Score) {
			lbl += " (tie)"
		}
		names[i] = lbl
		statuses[i] = ranking[i].Status
		evictions[i] = ranking[i].Score.Evicted
		moves[i] = ranking[i].Score.Moved
		durations[i] = ranking[i].DurationMs
	}

	placed := baseline.PlacedByPriority
	if best.Name != "baseline" {
		placed = best.Score.PlacedByPriority
	}

	klog.InfoS(
		msg(label, "solver leaderboard"),
		"ranking", names,
		"status", statuses,
		"durationsUs", durations,
		"evictions", evictions,
		"moves", moves,
		"placedByPri", placed,
		"prevPlacedByPri", baseline.PlacedByPriority,
	)
}

// scorePlan computes final Score from the snapshot given to the solver:
//   - placed_by_priority: number of pods that were placed for each priority
//   - evicted:            number of pods that were evicted
//   - moved:              number of pods that were moved to a different node
func scorePlan(in PlannerInput, out *PlannerOutput) PlannerScore {
	if out == nil {
		return PlannerScore{}
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
		if plm.Node == "" {
			continue
		}
		if _, known := afterWhere[plm.UID]; known {
			afterWhere[plm.UID] = plm.Node
		}
	}

	// Evicted
	evicted := make(map[types.UID]struct{}, len(out.Evictions))
	for _, e := range out.Evictions {
		evicted[e.UID] = struct{}{}
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

	return PlannerScore{
		PlacedByPriority: placedByPri,
		Evicted:          len(evicted),
		Moved:            moves,
	}
}

// toPod converts a Pod to a SolverPod.
func toPod(p *v1.Pod, node string) PlannerPod {
	return PlannerPod{
		UID:         p.UID,
		Namespace:   p.Namespace,
		Name:        p.Name,
		ReqCPUm:     getPodCPURequest(p),
		ReqMemBytes: getPodMemoryRequest(p),
		Priority:    getPodPriority(p),
		Node:        node,
	}
}

// exportSolverStatsToConfigMap exports a compact run record to the stats ConfigMap.
// Only runs when `hadFeasible` is true.
func (pl *SharedState) exportSolverStatsToConfigMap(
	ctx context.Context,
	strategy string,
	baseline PlannerScore,
	best string,
	attempts []PlannerResult,
	err string,
) {
	entry := ExportedPlannerStats{
		TimestampNs: time.Now().UnixNano(),
		BestName:    best,
		Error:       err,
		Baseline:    baseline,
		Attempts:    attempts,
	}
	pl.appendSolverStatsCM(ctx, entry)
	klog.V(MyV).InfoS(msg(strategy, "exported solver stats"),
		"attempts", len(attempts),
		"bestAttempt", best,
		"error", err,
	)
}

var appendSolverStatsCMHook func(pl *SharedState, ctx context.Context, entry ExportedPlannerStats)

func (pl *SharedState) appendSolverStatsCM(ctx context.Context, entry ExportedPlannerStats) {
	// Allow unit tests to intercept ConfigMap writes and avoid real K8s clients.
	if appendSolverStatsCMHook != nil {
		appendSolverStatsCMHook(pl, ctx, entry)
		return
	}

	cli := pl.Handle.ClientSet()
	if cli == nil {
		klog.V(1).Info("no clientset; skip stats CM")
		return
	}
	doc := ConfigMapDoc{
		Namespace: SystemNamespace,
		Name:      SolverStatsConfigMapName,
		LabelKey:  SolverStatsConfigMapLabelKey,
		DataKey:   SolverStatsConfigMapLabelKey + ".json",
	}

	err := mutateJson(
		ctx,
		cli.CoreV1(),
		func(ns string) clientv1.ConfigMapNamespaceLister {
			return pl.Handle.SharedInformerFactory().Core().V1().ConfigMaps().Lister().ConfigMaps(ns)
		},
		doc,
		func(existing []ExportedPlannerStats) ([]ExportedPlannerStats, error) {
			return append(existing, entry), nil
		},
	)
	if apierrors.IsNotFound(err) {
		_ = doc.ensureJson(ctx, cli.CoreV1(), []ExportedPlannerStats{entry})
		return
	}
	if err != nil {
		klog.ErrorS(err, "append solver stats failed")
	}
}
