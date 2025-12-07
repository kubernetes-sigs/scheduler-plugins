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
// in a single pass. It:
//   - Filters to usable nodes only
//   - Adds all RUNNING pods that are bound to usable nodes
//   - Adds all PENDING pods
//   - Excludes the preemptor from Pods (it is provided via in.Preemptor)
//   - Marks preemptor as pending in the input
func (pl *SharedState) buildSolverInput(
	nodes []*v1.Node,
	pods []*v1.Pod,
	preemptor *v1.Pod,
) (SolverInput, error) {

	in := SolverInput{
		IgnoreAffinity: true,
		LogProgress:    SolverLogProgress,
		Nodes:          make([]SolverNode, 0, len(nodes)),
		Pods:           make([]SolverPod, 0, len(pods)),
		TimeoutMs:      0, // filled by caller
	}

	// ----- Nodes: keep only usable -----
	usable := make(map[string]bool, len(nodes))
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
	if len(in.Nodes) == 0 {
		return SolverInput{}, ErrNoUsableNodes
	}

	// ----- Preemptor: include as pending (not in Pods list) -----
	preUID := ""
	if preemptor != nil {
		pre := toSolverPod(preemptor, "")
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
			sp := toSolverPod(p, "")
			if isPodProtected(p) {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}

		default:
			// running → include only if bound to a usable node
			if !usable[where] {
				continue
			}
			sp := toSolverPod(p, where)
			if isPodProtected(p) {
				sp.Protected = true
			}
			if !seen[sp.UID] {
				in.Pods = append(in.Pods, sp)
				seen[sp.UID] = true
			}
		}
	}

	return in, nil
}

// buildBaselineScore computes the baseline score from the solver input.
func buildBaselineScore(in SolverInput) *SolverScore {
	placedByPri := map[string]int{}
	for _, sp := range in.Pods {
		if sp.Node == "" {
			continue // pending doesn't count into "placed"
		}
		pr := strconv.Itoa(int(sp.Priority))
		placedByPri[pr] = placedByPri[pr] + 1
	}
	return &SolverScore{
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
// 1) More placed per priority (lexicographic map compare)
// 2) Fewer evictions
// 3) Fewer moves
// Returns 1 if suggested is better, -1 if worse, 0 if equal.
// Returns as soon as a difference is found.
func isImprovement(baseline, suggested SolverScore) int {
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
	// meaning that placing more high-prio pods is better
	// (even if it means placing fewer low-prio pods, we trust the solver to optimize that)
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

// hasSolverFeasibleResult checks if the solver output is feasible.
// OPTIMAL means the solution is perfect and meets all constraints
// (Note there can be multiple optimal solutions and that the solver is non-deterministic).
// FEASIBLE means the solution is not optimal (not perfect) but still meets all constraints.
func hasSolverFeasibleResult(status string) bool {
	return status != "" && (status == "OPTIMAL" || status == "FEASIBLE")
}

// planApplicable checks whether a SolverOutput (plan) can still be safely
// applied on the current cluster state. It allows unrelated drift and only
// insists that the concrete preconditions for the plan still hold.
func (pl *SharedState) planApplicable(out *SolverOutput, nodes []*v1.Node, pods []*v1.Pod) (bool, string) {
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
	pByUID := podsByUID(pods)

	addUse := func(node string, cpu, mem int64) {
		u := used[node]
		u.cpu += cpu
		u.mem += mem
		used[node] = u
	}

	// Tally current usage
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil || getPodAssignedNodeName(p) == "" {
			continue
		}
		addUse(getPodAssignedNodeName(p), getPodCPURequest(p), getPodMemoryRequest(p))
	}

	// Simulate the plan on top of current usage:
	// - Evictions free resources on their current node
	for _, e := range out.Evictions {
		p := pByUID[e.Pod.UID]
		if p == nil || getPodAssignedNodeName(p) == "" {
			// Already gone or pending now: keep going.
			continue
		}
		// If a node became unusable, fail.
		if !usable[getPodAssignedNodeName(p)] {
			return false, fmt.Sprintf("evict node now unusable: %s", getPodAssignedNodeName(p))
		}
		addUse(getPodAssignedNodeName(p), -getPodCPURequest(p), -getPodMemoryRequest(p))
	}

	// - Moves/placements: check pod still where we expect (pending or src), then add to dst
	for _, np := range out.Placements {
		p := pByUID[np.Pod.UID]
		if p == nil || p.DeletionTimestamp != nil {
			return false, fmt.Sprintf("pod vanished: %s", mergeNsName(np.Pod.Namespace, np.Pod.Name))
		}
		// Source must still be consistent enough:
		//   - if it was a move (FromNode != ""), pod should still be on that source
		//   - if it was a new/pending placement (FromNode == ""), pod should still be pending
		if np.FromNode != "" {
			if getPodAssignedNodeName(p) != np.FromNode {
				return false, fmt.Sprintf("move precondition changed for %s/%s: was on %q, now on %q",
					p.Namespace, p.Name, np.FromNode, getPodAssignedNodeName(p))
			}
			// remove from src (already accounted by evictions? No — moves are distinct)
			addUse(np.FromNode, -getPodCPURequest(p), -getPodMemoryRequest(p))
		} else {
			// pending expected
			if getPodAssignedNodeName(p) != "" {
				return false, fmt.Sprintf("pending precondition changed for %s/%s: now bound to %q",
					p.Namespace, p.Name, getPodAssignedNodeName(p))
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

// copy to a "summary": drop Output/CmpBase and fill Status from Output.
func summarizeAttempt(r SolverResult) SolverResult {
	status := r.Status
	if status == "" && r.Output != nil {
		status = r.Output.Status
	}
	return SolverResult{
		Name:       r.Name,
		Status:     status,
		DurationMs: r.DurationMs,
		Score:      r.Score,
		Stages:     r.Stages,
	}
}

// logLeaderboard prints a compact solver leaderboard relative to baseline.
// It groups attempts as better/equal/worse vs baseline and tags adjacent ties.
func logLeaderboard(
	label string,
	attempts []SolverResult,
	baseline SolverScore,
	best SolverResult,
) {
	if len(attempts) == 0 {
		// still include baseline-only view
		attempts = nil
	}

	// Classify relative to baseline while preserving attempt order inside groups.
	var better, equal, worse []SolverResult
	for _, r := range attempts {
		rr := SolverResult{
			Name:       r.Name,
			DurationMs: r.DurationMs,
			Score:      r.Score,
			Status:     r.Status,
			CmpBase:    isImprovement(baseline, r.Score),
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

	// Include the baseline as an entry, and make it the first among equals.
	baselineEntry := SolverResult{
		Name:       "baseline",
		Status:     "BASELINE",
		DurationMs: 0,
		Score:      baseline,
		CmpBase:    0,
	}
	equal = append([]SolverResult{baselineEntry}, equal...)

	ranking := append(append(better, equal...), worse...)

	// Tie helper
	tied := func(a, b SolverScore) bool {
		return isImprovement(a, b) == 0 && isImprovement(b, a) == 0
	}

	// Build log arrays
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

// scoreSolution computes final Score from the snapshot given to the solver:
//   - placed_by_priority: number of pods that were placed for each priority
//   - evicted:            number of pods that were evicted
//   - moved:              number of pods that were moved to a different node
func scoreSolution(in SolverInput, out *SolverOutput) SolverScore {
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

// exportSolverStatsToConfigMap exports a compact run record to the stats ConfigMap.
// Only runs when `hadFeasible` is true.
func (pl *SharedState) exportSolverStatsToConfigMap(
	ctx context.Context,
	strategy string,
	baseline *SolverScore,
	best string,
	attempts []SolverResult,
	err string,
) {
	// Build summarized attempts to keep payload lean
	slim := make([]SolverResult, 0, len(attempts))
	for _, r := range attempts {
		slim = append(slim, summarizeAttempt(r))
	}

	entry := ExportedSolverStats{
		TimestampNs: time.Now().UnixNano(),
		BestName:    best,
		Error:       err,
		Baseline:    baseline,
		Attempts:    slim,
	}
	pl.appendSolverStatsCM(ctx, entry)
	klog.V(MyV).InfoS(msg(strategy, "exported solver stats"),
		"attempts", len(slim),
		"bestAttempt", best,
		"error", err,
	)
}

var appendSolverStatsCMHook func(pl *SharedState, ctx context.Context, entry ExportedSolverStats)

func (pl *SharedState) appendSolverStatsCM(ctx context.Context, entry ExportedSolverStats) {
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
		func(existing []ExportedSolverStats) ([]ExportedSolverStats, error) {
			return append(existing, entry), nil
		},
	)
	if apierrors.IsNotFound(err) {
		_ = doc.ensureJson(ctx, cli.CoreV1(), []ExportedSolverStats{entry})
		return
	}
	if err != nil {
		klog.ErrorS(err, "append solver stats failed")
	}
}
