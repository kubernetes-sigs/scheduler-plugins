// postfilter_plugin.go

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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ---------------------------- PostFilter ----------------------------

func (pl *MyCrossNodePreemption) PostFilter(
	ctx context.Context,
	state *framework.CycleState,
	pending *v1.Pod,
	_ framework.NodeToStatusMap,
) (*framework.PostFilterResult, *framework.Status) {
	// Don't allow another run of PostFilter if an active plan exists
	sp, _ := pl.getActivePlan()
	if sp != nil && !sp.Completed {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "active plan exists")
	}

	klog.InfoS("PostFilter start", "pending pod", klog.KObj(pending),
		"cpu(m)", getPodCPURequest(pending),
		"mem(MiB)", bytesToMiB(getPodMemoryRequest(pending)),
	)

	// Early cluster-sum to avoid running solver if we already know that
	// moving/rejecting lower priority pods won't fit the preemptor.
	if ok, reason, err := pl.clusterTotalCapacity(pending); err != nil {
		klog.ErrorS(err, "Early cluster capacity check failed")
		return nil, framework.NewStatus(framework.Error, "capacity check failed")
	} else if !ok {
		klog.InfoS("Early capacity check: unschedulable regardless of solver", "reason", reason)
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, reason)
	}

	// Run solver
	solveCtx, cancel := context.WithTimeout(ctx, PythonSolverTimeout)
	defer cancel()
	start := time.Now()
	out, err := pl.runPythonOptimizer(solveCtx, pending, PythonSolverTimeout)
	if err != nil {
		klog.ErrorS(err, "PostFilter: optimizer error", "took", time.Since(start))
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}
	klog.InfoS("PostFilter: solver executed", "status", out.Status, "took", time.Since(start))
	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}
	if plan == nil || (len(plan.PodMovements) == 0 && len(plan.VictimsToEvict) == 0 && out.NominatedNode == "") {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "no actionable plan")
	}

	// Export active plan to ConfigMap for debugging purposes
	cmName, err := pl.exportPlanToConfigMap(ctx, plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "PostFilter: Failed to export plan to ConfigMap (continuing with in-memory only)")
	}

	// Build StoredPlan in-memory (same data as ConfigMap)
	lite, byName, rsDesired, err := pl.materializePlanDocs(plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "PostFilter: failed to materialize plan docs")
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}

	inMem := &StoredPlan{
		Completed:              false,
		GeneratedAt:            time.Now().UTC(),
		PluginVersion:          Version,
		PendingPod:             fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:             string(pending.UID),
		TargetNode:             plan.TargetNode,
		SolverOutput:           out,
		Plan:                   lite,
		PlacementsByName:       byName,
		WorkloadDesiredPerNode: rsDesired,
	}

	pl.setActivePlan(inMem, cmName)

	// Arm a one-shot timeout. If the plan is still active when TTL elapses,
	// it will be deactivated. No completion polling involved.
	_, planID := pl.getActivePlan()
	pl.startPlanTimeout(ctx, planID, cmName, PlanExecutionTTL)

	klog.InfoS("PostFilter: executing plan",
		"targetNode", plan.TargetNode,
		"movements", len(plan.PodMovements),
		"evictions", len(plan.VictimsToEvict),
	)
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("PostFilter: movement plan",
			"idx_move", i+1, "pod", podRef(mv.Pod),
			"from", mv.FromNode, "to", mv.ToNode,
			"cpu(m)", mv.CPURequest, "mem(MiB)", bytesToMiB(mv.MemoryRequest),
		)
	}
	for i, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("PostFilter: eviction plan",
			"idx_evict", i+1, "pod", podRef(v),
			"node", v.Spec.NodeName,
			"cpu(m)", getPodCPURequest(v), "mem(MiB)", bytesToMiB(getPodMemoryRequest(v)),
		)
	}

	// Execute plan (evictions/recreates)
	if err := pl.executePlan(ctx, plan); err != nil {
		klog.ErrorS(err, "PostFilter: plan execution failed")
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}

	klog.InfoS("PostFilter: plan executed successfully")

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{NominatedNodeName: plan.TargetNode},
	}, framework.NewStatus(framework.Success, "")
}

// ----------------------- Solver bridge ----------------------------

func (pl *MyCrossNodePreemption) runPythonOptimizer(
	ctx context.Context,
	pending *v1.Pod,
	timeout time.Duration,
) (*SolverOutput, error) {

	nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	in := SolverInput{
		TimeoutMs:      timeout.Milliseconds(),
		IgnoreAffinity: true,
		Preemptor:      toSolverPod(pending, ""),
	}

	usable := map[string]bool{}
	for _, ni := range nodes {
		if !isNodeUsable(ni) {
			continue
		}
		in.Nodes = append(in.Nodes, SolverNode{
			Name: ni.Node().Name,
			CPU:  ni.Allocatable.MilliCPU,
			RAM:  ni.Allocatable.Memory,
		})
		usable[ni.Node().Name] = true
	}

	for _, ni := range nodes {
		if !isNodeUsable(ni) {
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
	klog.V(2).InfoS("PostFilter: Solver input detail", "raw", string(raw))
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "python3", PythonSolverPath)
	cmd.Stdin = bytes.NewReader(raw)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf

	if err := cmd.Run(); err != nil {
		klog.ErrorS(err, "PostFilter: python solver failed", "stderr", errBuf.String())
		return nil, fmt.Errorf("solver run: %w", err)
	}

	var out SolverOutput
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("PostFilter: decode solver output: %w", err)
	}
	if out.Status != "OK" {
		return &out, fmt.Errorf("PostFilter: solver status: %s", out.Status)
	}
	return &out, nil
}

func toSolverPod(p *v1.Pod, where string) SolverPod {
	return SolverPod{
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
	out *SolverOutput,
	pending *v1.Pod,
) (*PodAssignmentPlan, error) {

	if out.NominatedNode == "" {
		return nil, fmt.Errorf("PostFilter: no nominated node for pending pod")
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
	out *SolverOutput,
	pending *v1.Pod,
) (string, error) {
	// Build the lite plan from the concrete plan we execute.
	litePlan := PodAssignmentPlanLite{TargetNode: plan.TargetNode}
	for _, mv := range plan.PodMovements {
		litePlan.Movements = append(litePlan.Movements, MovementLite{
			Pod:      PodRefLite{Namespace: mv.Pod.Namespace, Name: mv.Pod.Name, UID: string(mv.Pod.UID)},
			FromNode: mv.FromNode,
			ToNode:   mv.ToNode,
			CPUm:     mv.CPURequest,
			MemBytes: mv.MemoryRequest,
		})
	}
	for _, v := range plan.VictimsToEvict {
		litePlan.Evictions = append(litePlan.Evictions, PodRefLite{
			Namespace: v.Namespace, Name: v.Name, UID: string(v.UID),
		})
	}

	// Single source of truth for byName + RSDesiredPerNode.
	// Ensure materializePlanDocs skips uid == pending.UID and uses ns/name for standalone.
	litePlan, byName, rsDesired, err := pl.materializePlanDocs(plan, out, pending)
	if err != nil {
		return "", err
	}

	doc := &StoredPlan{
		Completed:              false,
		CompletedAt:            nil,
		GeneratedAt:            time.Now().UTC(),
		PluginVersion:          Version,
		PendingPod:             fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:             string(pending.UID),
		TargetNode:             plan.TargetNode,
		SolverOutput:           out,
		Plan:                   litePlan,
		PlacementsByName:       byName,    // keys are "ns/name"
		WorkloadDesiredPerNode: rsDesired, // pending pod excluded
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	name := fmt.Sprintf("%s%s-%d", ConfigMapNamePrefix, pending.UID, time.Now().Unix())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ConfigMapNamespace,
			Labels:    map[string]string{ConfigMapLabelKey: "true"},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}

	_ = pl.pruneOldPlans(ctx, 20)
	return name, nil
}

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan) error {
	// Collect unique target pods (moves + evictions).
	var targets []*v1.Pod
	seen := map[string]bool{}
	for _, mv := range plan.PodMovements {
		key := mv.Pod.Namespace + "/" + mv.Pod.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, mv.Pod)
		}
	}
	for _, v := range plan.VictimsToEvict {
		key := v.Namespace + "/" + v.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, v)
		}
	}

	// 1) evict/wait pods (batched)  --->  **NEW**
	if len(targets) > 0 {
		klog.V(2).InfoS("Evicting/awaiting eviction of targeted pods", "count", len(targets))

		// We now evict *all* targets, regardless of owner.
		// (Controller-owned will be recreated by their controller.)
		for _, p := range targets {
			if err := pl.evictPod(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict pod %s: %w", podRef(p), err)
			}
		}

		// Wait for all targeted pods to actually disappear
		if err := pl.waitPodsGone(ctx, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// 2) recreate standalone pods (no bind)
	for _, mv := range plan.PodMovements {
		if _, ok := topWorkload(mv.Pod); ok {
			klog.V(2).InfoS("Skipping workload-owned move recreate (controller will recreate)", "pod", podRef(mv.Pod), "to", mv.ToNode)
			continue
		}
		klog.V(2).InfoS("Recreating moved standalone pod (no bind)", "pod", podRef(mv.Pod))
		if err := pl.recreatePod(ctx, mv.Pod, ""); err != nil {
			return fmt.Errorf("recreate moved pod %s: %w", podRef(mv.Pod), err)
		}
	}
	for _, v := range plan.VictimsToEvict {
		if _, ok := topWorkload(v); ok {
			klog.V(2).InfoS("Skipping workload-owned eviction recreate (controller will recreate)", "pod", podRef(v))
			continue
		}
		klog.V(2).InfoS("Recreating evicted standalone pod (no bind)", "pod", podRef(v))
		if err := pl.recreatePod(ctx, v, ""); err != nil {
			return fmt.Errorf("recreate evicted pod %s: %w", podRef(v), err)
		}
	}

	return nil
}

// ---------------------------- Deletion helpers ----------------------------

func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	return wait.PollUntilContextTimeout(ctx, EvictionPollInterval, EvictionPollTimeout, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}

		// iterate over a snapshot of keys so we can delete while iterating
		for k := range remaining {
			got, err := pl.client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				delete(remaining, k) // gone
				continue
			}
			if err != nil {
				// transient error: ignore and retry
				return false, nil
			}
			if string(got.UID) != k.uid {
				// replacement/new instance => original is gone
				delete(remaining, k)
				continue
			}
			// else: original still present; keep it in the set
		}

		if len(remaining) == 0 {
			return true, nil
		}
		klog.V(2).InfoS("Waiting for targeted pods to disappear",
			"remaining", len(remaining))
		return false, nil
	})
}

// Recreate a standalone pod without direct binding
func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.NodeName = "" // no direct binding
	newp.Spec.NodeSelector = map[string]string{}

	if _, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	return nil
}

// clusterTotalCapacity returns true if, cluster-wide, the sum of current
// free headroom + reclaimable from strictly lower-priority pods is enough to
// satisfy the pending pod's CPU AND memory requests. If not, we can bail out
// before the solver, since we know no solution can exist.
func (pl *MyCrossNodePreemption) clusterTotalCapacity(pending *v1.Pod) (bool, string, error) {
	nodes, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return false, "snapshot error", err
	}

	wantCPU := getPodCPURequest(pending)
	wantMem := getPodMemoryRequest(pending)
	pPri := getPodPriority(pending)

	var totalCPU, totalMem int64

	for _, ni := range nodes {
		if !isNodeUsable(ni) {
			continue
		}

		// 1) current free headroom on the node
		freeCPU := ni.Allocatable.MilliCPU - ni.Requested.MilliCPU
		freeMem := ni.Allocatable.Memory - ni.Requested.Memory
		if freeCPU < 0 {
			freeCPU = 0
		}
		if freeMem < 0 {
			freeMem = 0
		}

		// 2) reclaimable from strictly lower-priority pods on this node
		var recCPU, recMem int64
		for _, pi := range ni.Pods {
			p := pi.Pod
			// strictly lower priority than the pending pod
			if getPodPriority(p) >= pPri {
				continue
			}
			// avoid counting system/static pods as reclaimable
			if p.Namespace == "kube-system" {
				continue
			}
			recCPU += getPodCPURequest(p)
			recMem += getPodMemoryRequest(p)
		}
		totalCPU += freeCPU + recCPU
		totalMem += freeMem + recMem
	}
	if totalCPU >= wantCPU && totalMem >= wantMem {
		return true, "", nil
	}
	reason := fmt.Sprintf(
		"insufficient cluster capacity: need cpu=%dm mem=%dMiB; have cpu=%dm mem=%dMiB",
		wantCPU, bytesToMiB(wantMem),
		totalCPU, bytesToMiB(totalMem),
	)
	return false, reason, nil
}

func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			// Preconditions ensure we evict the exact instance we planned for.
			UID: pod.UID,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		},
	}
	return pl.client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
}
