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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

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
	if ok, reason, err := pl.clusterCapacityCheck(pending); err != nil {
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
		Completed:        false,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode,
		SolverOutput:     out,
		Plan:             lite,
		PlacementsByName: byName,
		RSDesiredPerNode: rsDesired,
	}

	pl.setActivePlan(inMem, cmName)

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
		Completed:        false,
		CompletedAt:      nil,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		PendingPod:       fmt.Sprintf("%s/%s", pending.Namespace, pending.Name),
		PendingUID:       string(pending.UID),
		TargetNode:       plan.TargetNode,
		SolverOutput:     out,
		Plan:             litePlan,
		PlacementsByName: byName,    // keys are "ns/name"
		RSDesiredPerNode: rsDesired, // pending pod excluded
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

// prevDeletionCosts maps "ns/name" -> original value pointer.
// nil => annotation absent; non-nil => original string value.
type prevDeletionCosts map[string]*string

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers after scale restore.
// Pending pod is *not* bound here; Filter constrains it to the target node
// and the default scheduler performs the bind.
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

	// Per-ReplicaSet deltas (negative for initial scale down).
	rsDeltas := map[struct{ ns, name string }]int32{}

	// 1) deletion-cost annotations (remember previous values)
	var prevCosts prevDeletionCosts
	if len(targets) > 0 {
		klog.V(2).InfoS("Setting deletion-cost annotations for targets and siblings", "targets", len(targets))
		var err error
		prevCosts, err = pl.annotateDeletionCosts(ctx, targets, DeletionCostTarget, DeletionCostKeep, rsDeltas)
		if err != nil {
			return fmt.Errorf("annotate deletion-cost: %w", err)
		}
	}

	// 2) scale down owners
	if len(rsDeltas) > 0 {
		klog.V(2).InfoS("Scaling down owners for targeted ReplicaSets", "sets", len(rsDeltas))
		for k, d := range rsDeltas {
			if d == 0 {
				continue
			}
			if err := pl.bumpOwnerScale(ctx, k.ns, k.name, d); err != nil {
				return fmt.Errorf("scale down %s/%s by %d: %w", k.ns, k.name, d, err)
			}
		}
	}

	// 3) delete/wait pods (batched)
	if len(targets) > 0 {
		klog.V(2).InfoS("Deleting/awaiting deletion of targeted pods", "count", len(targets))

		// Split: RS-owned (controller deletes) vs standalone (we delete)
		var rsOwned, standalone []*v1.Pod
		for _, p := range targets {
			if _, isRS := owningRS(p); isRS {
				rsOwned = append(rsOwned, p)
			} else {
				standalone = append(standalone, p)
			}
		}

		// Issue deletes for all standalone first
		for _, p := range standalone {
			if err := pl.deletePod(ctx, p); err != nil {
				return fmt.Errorf("delete non-RS pod %s: %w", podRef(p), err)
			}
		}

		// Now wait once for all (RS-owned + standalone we just deleted)
		var waitAll []*v1.Pod
		waitAll = append(waitAll, rsOwned...)
		waitAll = append(waitAll, standalone...)
		if err := pl.waitPodsGone(ctx, waitAll); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// 4) restore owner scales (no bind)
	if len(rsDeltas) > 0 {
		klog.V(2).InfoS("Restoring owner scales", "sets", len(rsDeltas))
		for k, d := range rsDeltas {
			if d == 0 {
				continue
			}
			if err := pl.bumpOwnerScale(ctx, k.ns, k.name, -d); err != nil {
				klog.ErrorS(err, "Failed to restore owner scale", "rs", fmt.Sprintf("%s/%s", k.ns, k.name), "delta", -d)
			}
		}
		// Wait for RS-owned pods to be
	}

	// 5) recreate standalone pods (no bind)
	for _, mv := range plan.PodMovements {
		if _, isRS := owningRS(mv.Pod); isRS {
			klog.V(2).InfoS("Skipping RS-owned move (controller will recreate)", "pod", podRef(mv.Pod), "to", mv.ToNode)
			continue
		}
		klog.V(2).InfoS("Recreating moved standalone pod (no bind)", "pod", podRef(mv.Pod))
		if err := pl.recreatePod(ctx, mv.Pod, ""); err != nil {
			return fmt.Errorf("recreate moved pod %s: %w", podRef(mv.Pod), err)
		}
	}
	for _, v := range plan.VictimsToEvict {
		if _, isRS := owningRS(v); isRS {
			klog.V(2).InfoS("Skipping RS-owned eviction recreate (controller will recreate)", "pod", podRef(v))
			continue
		}
		klog.V(2).InfoS("Recreating evicted standalone pod (no bind)", "pod", podRef(v))
		if err := pl.recreatePod(ctx, v, ""); err != nil {
			return fmt.Errorf("recreate evicted pod %s: %w", podRef(v), err)
		}
	}

	// 6) restore original deletion-costs
	if len(prevCosts) > 0 {
		klog.V(2).InfoS("Restoring previous pod-deletion-cost annotations", "count", len(prevCosts))
		pl.restoreDeletionCosts(ctx, prevCosts)
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

	return wait.PollUntilContextTimeout(ctx, PollInterval, PollTimeout, true, func(ctx context.Context) (bool, error) {
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

func (pl *MyCrossNodePreemption) deletePod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
	pre := &metav1.Preconditions{UID: &pod.UID}
	if derr := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		Preconditions:      pre,
	}); derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("delete pod %s: %w", podRef(pod), derr)
	}
	return nil
}

// ---------------------------- Deletion-cost helpers ----------------------------

// annotateDeletionCosts sets low deletion-cost on target pods and high deletion-cost on their
// RS siblings; it also fills rsDeltas with negative counts for initial scale-down.
// It RETURNS the *previous* values of the annotation for every pod it touched.
func (pl *MyCrossNodePreemption) annotateDeletionCosts(
	ctx context.Context,
	targets []*v1.Pod,
	targetCost, siblingCost int,
	rsDeltas map[struct{ ns, name string }]int32,
) (prevDeletionCosts, error) {
	type key struct{ ns, name string }
	group := map[key][]*v1.Pod{}
	prev := prevDeletionCosts{}

	// Group targets per RS and populate rsDeltas
	for _, p := range targets {
		if rsName, ok := owningRS(p); ok {
			k := key{ns: p.Namespace, name: rsName}
			group[k] = append(group[k], p)
			rsDeltas[k] -= 1
		}
	}

	// For each RS: set target (low) and sibling (high); record previous for all touched
	for k, pods := range group {
		rs, err := pl.client.AppsV1().ReplicaSets(k.ns).Get(ctx, k.name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get rs %s/%s: %w", k.ns, k.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("selector for rs %s/%s: %w", k.ns, k.name, err)
		}
		podList, err := pl.client.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, fmt.Errorf("list rs pods %s/%s: %w", k.ns, k.name, err)
		}

		targetSet := map[string]struct{}{}
		for _, p := range pods {
			targetSet[p.Name] = struct{}{}
		}

		// Low cost on targets — record previous and patch
		for _, p := range pods {
			key := p.Namespace + "/" + p.Name
			if _, seen := prev[key]; !seen {
				if p.Annotations != nil {
					if v, ok := p.Annotations[DeletionCostAnnotation]; ok {
						vv := v
						prev[key] = &vv
					} else {
						prev[key] = nil
					}
				} else {
					prev[key] = nil
				}
			}
			if err := pl.setDeletionCost(ctx, p.Namespace, p.Name, targetCost); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("set deletion-cost target %s/%s: %w", p.Namespace, p.Name, err)
			}
		}

		// High cost on siblings — record previous and patch
		for i := range podList.Items {
			sib := &podList.Items[i]
			if !isControlledByRS(sib, k.name) {
				continue
			}
			if _, isTarget := targetSet[sib.Name]; isTarget {
				continue
			}
			key := sib.Namespace + "/" + sib.Name
			if _, seen := prev[key]; !seen {
				if sib.Annotations != nil {
					if v, ok := sib.Annotations[DeletionCostAnnotation]; ok {
						vv := v
						prev[key] = &vv
					} else {
						prev[key] = nil
					}
				} else {
					prev[key] = nil
				}
			}
			if err := pl.setDeletionCost(ctx, sib.Namespace, sib.Name, siblingCost); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("set deletion-cost keep %s/%s: %w", sib.Namespace, sib.Name, err)
			}
		}
	}
	return prev, nil
}

func (pl *MyCrossNodePreemption) setDeletionCost(ctx context.Context, ns, podName string, cost int) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%d"}}}`, DeletionCostAnnotation, cost))
	_, err := pl.client.CoreV1().Pods(ns).Patch(ctx, podName, types.StrategicMergePatchType, patch, metav1.PatchOptions{
		FieldManager: "my-crossnode-plugin",
	})
	return err
}

// restoreDeletionCosts tries to put back each pod's original value.
// If prev is nil => delete annotation. If not found => ignore.
func (pl *MyCrossNodePreemption) restoreDeletionCosts(ctx context.Context, prev prevDeletionCosts) {
	for key, old := range prev {
		ns, name := splitNSName(key) // key is "ns/name"
		var patch []byte
		if old == nil {
			// remove annotation
			patch = []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, DeletionCostAnnotation))
		} else {
			// restore exact prior value
			patch = []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, DeletionCostAnnotation, *old))
		}
		if _, err := pl.client.CoreV1().Pods(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{
			FieldManager: "my-crossnode-plugin",
		}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to restore deletion-cost", "pod", key)
		} else {
			klog.V(2).InfoS("Restored deletion-cost", "pod", key)
		}
	}
}

func (pl *MyCrossNodePreemption) bumpOwnerScale(ctx context.Context, ns, rsName string, delta int32) error {
	rs, err := pl.client.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var depName string
	for _, o := range rs.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "Deployment" {
			depName = o.Name
			break
		}
	}
	if depName != "" {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			sc, err := pl.client.AppsV1().Deployments(ns).GetScale(ctx, depName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			sc.Spec.Replicas += delta
			_, err = pl.client.AppsV1().Deployments(ns).UpdateScale(ctx, depName, sc, metav1.UpdateOptions{})
			return err
		})
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sc, err := pl.client.AppsV1().ReplicaSets(ns).GetScale(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		sc.Spec.Replicas += delta
		_, err = pl.client.AppsV1().ReplicaSets(ns).UpdateScale(ctx, rsName, sc, metav1.UpdateOptions{})
		return err
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

// clusterCapacityCheck returns true if, cluster-wide, the sum of current
// free headroom + reclaimable from strictly lower-priority pods is enough to
// satisfy the pending pod's CPU AND memory requests. If not, we can bail out
// before the solver, since we know no solution can exist.
func (pl *MyCrossNodePreemption) clusterCapacityCheck(pending *v1.Pod) (bool, string, error) {
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
			if _, isMirror := p.Annotations["kubernetes.io/config.mirror"]; isMirror {
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
