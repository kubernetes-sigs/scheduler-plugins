// pkg/mycrossnodepreemption/solver_cp.go
// Build tag optional: //go:build go_cpsat

package mycrossnodepreemption

import (
	"fmt" // CpModelProto / CpSolverResponse
	"math"
	"sort"
	"time"

	sppb "github.com/google/or-tools/ortools/sat_parameters_go_proto" // SatParameters

	cp "github.com/google/or-tools/ortools/sat/go" // the Go modeling API
)

func (pl *MyCrossNodePreemption) runSolverCP(in SolverInput) *SolverOutput {
	nodes := in.Nodes
	pods := in.Pods
	if len(nodes) == 0 {
		return &SolverOutput{Status: "NO_NODES"}
	}
	if len(pods) == 0 {
		return &SolverOutput{Status: "NO_PODS"}
	}

	start := time.Now()

	// -------- index helpers ----------
	nodeIndex := map[string]int{}
	for j, n := range nodes {
		nodeIndex[n.Name] = j
	}
	nCapCPU := func(j int) int64 { return int64(nodes[j].CapMilliCPU) }
	nCapMem := func(j int) int64 { return int64(nodes[j].CapBytes) }

	pReqCPU := func(i int) int64 { return int64(pods[i].ReqMilliCPU) }
	pReqMem := func(i int) int64 { return int64(pods[i].ReqBytes) }
	pPriority := func(i int) int { return int(pods[i].Priority) }
	pProtected := func(i int) bool { return pods[i].Protected }
	pNodeJ := func(i int) (int, bool) {
		if pods[i].Node == "" {
			return -1, false
		}
		j, ok := nodeIndex[pods[i].Node]
		return j, ok
	}

	// De-dup by UID preferring “has node” version (like Python).
	seen := map[string]int{}
	keep := make([]bool, len(pods))
	for i := range pods {
		uid := pods[i].UID
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; !ok {
			keep[i] = true
			seen[uid] = i
			continue
		}
		// Prefer record with a node if the saved one had none.
		prev := seen[uid]
		_, prevHas := pNodeJ(prev)
		_, curHas := pNodeJ(i)
		if curHas && !prevHas {
			keep[prev] = false
			keep[i] = true
			seen[uid] = i
		}
	}
	// Compact pods slice
	if len(seen) != len(pods) {
		cpPods := make([]*PodInfo, 0, len(seen))
		for i := range pods {
			if keep[i] || pods[i].UID == "" {
				cpPods = append(cpPods, pods[i])
			}
		}
		pods = cpPods
	}

	n := len(nodes)
	m := len(pods)

	// Preemptor mode
	singlePreemptor := false
	preIdx := -1
	prePri := 0
	if in.Preemptor != nil && in.Preemptor.UID != "" {
		singlePreemptor = true
		for i := range pods {
			if pods[i].UID == in.Preemptor.UID {
				preIdx = i
				prePri = pPriority(i)
				break
			}
		}
	}

	// Eligible nodes per pod
	eligible := make([][]int, m)
	locOf := make([]map[int]int, m)
	for i := 0; i < m; i++ {
		locOf[i] = make(map[int]int)
		for j := 0; j < n; j++ {
			if nCapCPU(j) >= pReqCPU(i) && nCapMem(j) >= pReqMem(i) {
				locOf[i][j] = len(eligible[i])
				eligible[i] = append(eligible[i], j)
			}
		}
	}

	// Build model
	model := cp.NewCpModelBuilder()

	// Decision vars
	placed := make([]cp.BoolVar, m)
	assign := make([][]cp.BoolVar, m)
	for i := 0; i < m; i++ {
		placed[i] = model.NewBoolVar().WithName(fmt.Sprintf("placed_%d", i))
		assign[i] = make([]cp.BoolVar, len(eligible[i]))
		for k := range eligible[i] {
			assign[i][k] = model.NewBoolVar().WithName(fmt.Sprintf("a_%d_%d", i, eligible[i][k]))
		}
	}

	// Capacity constraints (CPU + Mem per node)
	for j := 0; j < n; j++ {
		if nCapCPU(j) == 0 && nCapMem(j) == 0 {
			continue
		}
		exprCPU := cp.NewLinearExpr()
		exprMem := cp.NewLinearExpr()
		for i := 0; i < m; i++ {
			if pos, ok := locOf[i][j]; ok {
				exprCPU.AddTerm(assign[i][pos], pReqCPU(i))
				exprMem.AddTerm(assign[i][pos], pReqMem(i))
			}
		}
		model.AddLessOrEqual(exprCPU, cp.NewConstant(nCapCPU(j)))
		model.AddLessOrEqual(exprMem, cp.NewConstant(nCapMem(j)))
	}

	// Exactly-one if placed, else 0 if not eligible
	for i := 0; i < m; i++ {
		if len(eligible[i]) == 0 {
			model.AddEquality(placed[i], model.FalseVar())
			continue
		}
		sum := cp.NewLinearExpr().AddSum(assign[i]...)
		model.AddEquality(sum, placed[i])
	}

	// Protected pods that are running: must stay where they are (if eligible)
	for i := 0; i < m; i++ {
		orig, ok := pNodeJ(i)
		if !pProtected(i) || !ok {
			continue
		}
		if pos, ok2 := locOf[i][orig]; ok2 {
			// assign[i][pos] == 1
			model.AddEquality(assign[i][pos], model.TrueVar())
		} else {
			// Protected but cannot stay -> invalid model under our rules.
			return &SolverOutput{Status: "MODEL_INVALID"}
		}
	}

	// Single-preemptor must be placed.
	if singlePreemptor && preIdx >= 0 {
		sum := cp.NewLinearExpr().AddSum(assign[preIdx]...)
		model.AddEquality(sum, model.TrueVar())
	} else {
		// Batch mode: at least one pending must be placed (avoid empty solution)
		atLeastOne := []cp.BoolVar{}
		for i := 0; i < m; i++ {
			if _, running := pNodeJ(i); !running {
				atLeastOne = append(atLeastOne, placed[i])
			}
		}
		if len(atLeastOne) > 0 {
			model.AddAtLeastOne(atLeastOne...)
		}
	}

	// Non-degradation by priority: for each pr, placed(>=pr) >= baseline running(>=pr & placeable)
	priSet := map[int]struct{}{}
	for i := 0; i < m; i++ {
		priSet[pPriority(i)] = struct{}{}
	}
	priorities := make([]int, 0, len(priSet))
	for pr := range priSet {
		priorities = append(priorities, pr)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	isPlaceable := func(i int) bool { return len(eligible[i]) > 0 }
	for _, pr := range priorities {
		var idxGe []int
		baseGe := int64(0)
		for i := 0; i < m; i++ {
			if pPriority(i) >= pr {
				idxGe = append(idxGe, i)
				if _, running := pNodeJ(i); running && isPlaceable(i) {
					baseGe++
				}
			}
		}
		if len(idxGe) == 0 {
			continue
		}
		sum := cp.NewLinearExpr()
		for _, i := range idxGe {
			sum.Add(placed[i])
		}
		model.AddGreaterOrEqual(sum, cp.NewConstant(baseGe))
	}

	// --- Moves term (for objective) ---
	moveTerm := func(i int) cp.LinearExpr {
		orig, ok := pNodeJ(i)
		if !ok {
			// pending pod: "moved" iff placed -> same as placed
			return cp.NewLinearExpr().Add(placed[i])
		}
		pos, ok2 := locOf[i][orig]
		if !ok2 {
			// original node not eligible anymore -> any placement implies a move
			return cp.NewLinearExpr().Add(placed[i])
		}
		// moved = placed - assign_to_original
		return cp.NewLinearExpr().Add(placed[i]).AddTerm(assign[i][pos], -1)
	}

	// Weighted objective ≈ lexicographic: for each priority tier, maximize placed and penalize moves.
	// Let Wp >> Wm so "placed" dominates "moves". Use exponential weights per tier to mimic lexicographic across tiers.
	const Wp int64 = 1_000_000 // weight per placement unit
	const Wm int64 = 1         // weight per move (to be subtracted)
	obj := cp.NewLinearExpr()

	for t, pr := range priorities {
		// Bigger weight for higher tiers.
		wTier := int64(math.Pow(10, float64(len(priorities)-t))) // 10^(K - t)
		for i := 0; i < m; i++ {
			if pPriority(i) >= pr {
				obj.AddTerm(placed[i], Wp*wTier)
				mv := moveTerm(i)
				obj.AddWeightedSum([]cp.LinearArgument{mv}, []int64{-Wm * wTier})
			}
		}
	}
	model.Maximize(obj)

	// Hints (optional): keep current placements to reduce churn
	// (We only hint for running pods to "stay".)
	{
		h := cp.Hint{}
		for i := 0; i < m; i++ {
			if orig, ok := pNodeJ(i); ok {
				if pos, ok2 := locOf[i][orig]; ok2 {
					h = append(h, cp.HintPair{Var: assign[i][pos], Value: 1})
					h = append(h, cp.HintPair{Var: placed[i], Value: 1})
				}
			}
		}
		model.SetHint(h)
	}

	// Build and solve
	pb, err := model.Model()
	if err != nil {
		return &SolverOutput{Status: "MODEL_INVALID"}
	}

	params := &sppb.SatParameters{
		NumSearchWorkers:  int32(in.NumWorkers), // 0 => all cores
		MaxTimeInSeconds:  float64(max(0, int(in.TimeoutMs-0))) / 1000.0,
		RelativeGapLimit:  in.GapLimit,
		LogSearchProgress: in.LogProgress,
		LogSubsolver:      in.LogSubsolvers,
		// Keep other defaults; can be tuned as needed.
	}
	res, err := cp.SolveCpModelWithParameters(pb, params)
	if err != nil {
		// If CP-SAT errors, surface as UNKNOWN so caller can try next solver.
		return &SolverOutput{Status: "UNKNOWN"}
	}

	status := res.GetStatus().String() // FEASIBLE/OPTIMAL/…
	// Extract plan
	toName := func(j int) string { return nodes[j].Name }

	placements := make([]Placement, 0)
	evictions := make([]Eviction, 0)

	for i := 0; i < m; i++ {
		orig, hadOrig := pNodeJ(i)
		wasRunning := hadOrig
		isPlaced := cp.SolutionBooleanValue(res, placed[i])

		if wasRunning && !isPlaced {
			// Eviction
			evictions = append(evictions, Eviction{
				Pod:  PodRef{UID: pods[i].UID, Namespace: pods[i].Namespace, Name: pods[i].Name},
				Node: toName(orig),
			})
			continue
		}
		if isPlaced && len(eligible[i]) > 0 {
			var chosenJ = -1
			for k, j := range eligible[i] {
				if cp.SolutionBooleanValue(res, assign[i][k]) {
					chosenJ = j
					break
				}
			}
			if chosenJ < 0 {
				continue
			}
			moved := !wasRunning || chosenJ != orig
			if moved {
				from := ""
				if wasRunning {
					from = toName(orig)
				}
				placements = append(placements, Placement{
					Pod:      PodRef{UID: pods[i].UID, Namespace: pods[i].Namespace, Name: pods[i].Name},
					FromNode: from,
					ToNode:   toName(chosenJ),
				})
			}
		}
	}

	durMs := time.Since(start).Milliseconds()
	return &SolverOutput{
		Status:     status,
		DurationMs: durMs,
		Placements: placements,
		Evictions:  evictions,
		Stages:     nil, // this MVP uses a single pass objective (no per-tier stage timings)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
