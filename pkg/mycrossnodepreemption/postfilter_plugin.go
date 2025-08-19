package mycrossnodepreemption

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	pythonSolverPath    = "/opt/solver/main.py"
	pythonSolverTimeout = 60 * time.Second
)

// ---------------------------- PostFilter ----------------------------

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {

	klog.InfoS("PostFilter start", "pending pod", klog.KObj(pending),
		"cpu(m)", getPodCPURequest(pending),
		"mem(bytes)", getPodMemoryRequest(pending),
	)

	// Run solver
	solveCtx, cancel := context.WithTimeout(ctx, pythonSolverTimeout+5*time.Second)
	defer cancel()

	start := time.Now()
	out, err := pl.runPythonOptimizer(solveCtx, pending, pythonSolverTimeout)
	if err != nil {
		klog.ErrorS(err, "optimizer error", "took", time.Since(start))
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	klog.InfoS("Solver executed", "status", out.Status, "took", time.Since(start))

	plan, err := pl.translatePlanFromSolver(ctx, out, pending)
	if err != nil {
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	if plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		return nil, framework.NewStatus(framework.Unschedulable, "no actionable plan")
	}

	pl.logPlan(plan)

	// Export ACTIVE plan for Filter
	if cmName, err := pl.exportPlanToConfigMap(ctx, plan, out, pending); err != nil {
		klog.ErrorS(err, "Failed to export plan to ConfigMap (continuing)")
	} else {
		klog.V(2).InfoS("Exported active plan", "configMap", fmt.Sprintf("%s/%s", exportNamespace, cmName))
	}

	// Execute (standalone pods are recreated but NOT bound; RS via controllers)
	if err := pl.executePlan(ctx, plan, pending); err != nil {
		klog.ErrorS(err, "plan execution failed")
		return nil, framework.NewStatus(framework.Unschedulable, err.Error())
	}

	klog.V(2).InfoS("PostFilter completed")

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.TargetNode},
	}, framework.NewStatus(framework.Success, "")
}

// ---------------------------- Solver bridge ----------------------------

func (pl *MyCrossNodePreemption) runPythonOptimizer(
	ctx context.Context,
	pending *v1.Pod,
	timeout time.Duration,
) (*solverOutput, error) {

	nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	in := solverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptor:      toSolverPod(pending, ""),
	}

	usable := map[string]bool{}
	for _, ni := range nodes {
		if !isNodeUsableFor(pending, ni) {
			continue
		}
		in.Nodes = append(in.Nodes, solverNode{
			Name: ni.Node().Name,
			CPU:  ni.Allocatable.MilliCPU,
			RAM:  ni.Allocatable.Memory,
		})
		usable[ni.Node().Name] = true
	}

	for _, ni := range nodes {
		if !isNodeUsableFor(pending, ni) {
			continue
		}
		for _, pi := range ni.Pods {
			where := pi.Pod.Spec.NodeName
			if where != "" && !usable[where] {
				continue
			}
			sp := toSolverPod(pi.Pod, where)
			if pi.Pod.Namespace == "kube-system" {
				sp.Protected = true
			}
			in.Pods = append(in.Pods, sp)
		}
	}
	in.Pods = append(in.Pods, toSolverPod(pending, ""))

	raw, err := json.Marshal(in)
	klog.V(5).InfoS("Solver input detail", "raw", string(raw))
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "python3", pythonSolverPath)
	cmd.Stdin = bytes.NewReader(raw)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf

	if err := cmd.Run(); err != nil {
		klog.ErrorS(err, "python solver failed", "stderr", errBuf.String())
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out solverOutput
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode solver output: %w", err)
	}
	if out.Status != "OK" {
		return &out, fmt.Errorf("solver status: %s", out.Status)
	}
	return &out, nil
}

func toSolverPod(p *v1.Pod, where string) solverPod {
	return solverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU:       getPodCPURequest(p),
		RAM:       getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
	}
}

// ---------------------------- Plan translation / export / logging -----------

func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	ctx context.Context,
	out *solverOutput,
	pending *v1.Pod,
) (*PodAssignmentPlan, error) {

	if out.NominatedNode == "" {
		return nil, fmt.Errorf("no nominated node for pending pod")
	}

	all, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, err
	}
	podsByUID := map[string]*v1.Pod{}
	for _, ni := range all {
		for _, pi := range ni.Pods {
			podsByUID[string(pi.Pod.UID)] = pi.Pod
		}
	}
	podsByUID[string(pending.UID)] = pending

	plan := &PodAssignmentPlan{TargetNode: out.NominatedNode}

	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.VictimsToEvict = append(plan.VictimsToEvict, p)
		}
	}
	for uid, dest := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || uid == string(pending.UID) {
			continue
		}
		from := p.Spec.NodeName
		if from == dest || dest == "" {
			continue
		}
		plan.PodMovements = append(plan.PodMovements, PodMovement{
			Pod:           p,
			FromNode:      from,
			ToNode:        dest,
			CPURequest:    getPodCPURequest(p),
			MemoryRequest: getPodMemoryRequest(p),
		})
	}
	return plan, nil
}

func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	plan *PodAssignmentPlan,
	out *solverOutput,
	pending *v1.Pod,
) (string, error) {
	// lite
	lite := PodAssignmentPlanLite{TargetNode: plan.TargetNode}
	for _, mv := range plan.PodMovements {
		lite.Movements = append(lite.Movements, MovementLite{
			Pod:      PodRefLite{Namespace: mv.Pod.Namespace, Name: mv.Pod.Name, UID: string(mv.Pod.UID)},
			FromNode: mv.FromNode,
			ToNode:   mv.ToNode,
			CPUm:     mv.CPURequest,
			MemBytes: mv.MemoryRequest,
		})
	}
	for _, v := range plan.VictimsToEvict {
		lite.Evictions = append(lite.Evictions, PodRefLite{
			Namespace: v.Namespace, Name: v.Name, UID: string(v.UID),
		})
	}

	// uid -> pod
	podsByUID := map[string]*v1.Pod{}
	if all, err := pl.handle.SnapshotSharedLister().NodeInfos().List(); err == nil {
		for _, ni := range all {
			for _, pi := range ni.Pods {
				podsByUID[string(pi.Pod.UID)] = pi.Pod
			}
		}
	}
	podsByUID[string(pending.UID)] = pending

	byName := make(map[string]string)
	rsDesired := map[string]map[string]int{}

	for uid, node := range out.Placements {
		p, ok := podsByUID[uid]
		if !ok || p == nil {
			continue
		}
		if rsName, okRS := owningReplicaSet(p); okRS {
			k := rsKey(p.Namespace, rsName)
			if _, ok := rsDesired[k]; !ok {
				rsDesired[k] = map[string]int{}
			}
			rsDesired[k][node]++
		} else {
			// Standalone: planned node by *name*; Filter will enforce.
			byName[p.Name] = node
		}
	}

	doc := &StoredPlan{
		SchemaVersion:    "3",
		GeneratedAt:      time.Now().UTC(),
		Plugin:           Name,
		Version:          Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode,
		StopTheWorld:     true,
		Completed:        false,
		SolverOutput:     out,
		Plan:             lite,
		PlacementsByName: byName,
		RSDesiredPerNode: rsDesired,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	// ensure one active
	if err := pl.deactivateOldPlans(ctx); err != nil {
		klog.ErrorS(err, "deactivate old plans")
	}

	name := fmt.Sprintf("crossnode-plan-%s-%d", pending.UID, time.Now().Unix())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: exportNamespace,
			Labels: map[string]string{
				exportCMLabelKey:       exportCMLabelVal,
				exportCMLabelActiveKey: exportCMLabelActiveTrue,
				"pendingPod":           pending.Name,
				"pendingNS":            pending.Namespace,
			},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.client.CoreV1().ConfigMaps(exportNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return name, nil
}

func (pl *MyCrossNodePreemption) logPlan(plan *PodAssignmentPlan) {
	klog.InfoS("Execution plan",
		"targetNode", plan.TargetNode,
		"movements", len(plan.PodMovements),
		"evictions", len(plan.VictimsToEvict),
	)
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("Movement plan",
			"idx_move", i+1, "pod", podRef(mv.Pod),
			"from", mv.FromNode, "to", mv.ToNode,
			"cpu(m)", mv.CPURequest, "mem(bytes)", mv.MemoryRequest,
		)
	}
	for i, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Eviction plan",
			"idx_evict", i+1, "pod", podRef(v),
			"node", v.Spec.NodeName,
			"cpu(m)", getPodCPURequest(v), "mem(bytes)", getPodMemoryRequest(v),
		)
	}
}
