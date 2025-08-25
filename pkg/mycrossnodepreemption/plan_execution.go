// plan_execution.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	ConfigMapNamespace = "kube-system"
	ConfigMapLabelKey  = "crossnode-plan"
	PythonSolverPath   = "/opt/solver/main.py"
)

func (pl *MyCrossNodePreemption) setActivePlan(sp *StoredPlan, id string) {
	// 1) Copy desired per-node targets
	desired := map[string]map[string]int{}
	for rs, perNode := range sp.WkDesiredPerNode {
		inner := make(map[string]int, len(perNode))
		for n, want := range perNode {
			inner[n] = want
		}
		desired[rs] = inner
	}

	// 2) Current per RS/node + UID map
	cur := map[string]map[string]int{}
	podsByUID := map[string]*v1.Pod{}
	if live, err := pl.getPods(); err == nil {
		for _, p := range live {
			podsByUID[string(p.UID)] = p
			if wk, ok := topWorkload(p); ok && p.Spec.NodeName != "" {
				key := wk.String()
				if _, tracked := desired[key]; tracked {
					if _, ok := cur[key]; !ok {
						cur[key] = map[string]int{}
					}
					cur[key][p.Spec.NodeName]++
				}
			}
		}
	}

	// 3) planned OUT counts from plan doc
	plannedOut := map[string]map[string]int{}
	addOut := func(uid string) {
		if p := podsByUID[uid]; p != nil {
			if wk, ok := topWorkload(p); ok && p.Spec.NodeName != "" {
				key := wk.String()
				if _, tracked := desired[key]; !tracked {
					return
				}
				if _, ok := plannedOut[key]; !ok {
					plannedOut[key] = map[string]int{}
				}
				plannedOut[key][p.Spec.NodeName]++
			}
		}
	}
	for _, mv := range sp.Plan.Moves {
		addOut(mv.Pod.UID)
	}
	for _, ev := range sp.Plan.Evicts {
		addOut(ev.Pod.UID)
	}

	// 4) remaining = desired - current + plannedOut (>=0)
	rem := map[string]map[string]int{}
	for rs, perNode := range desired {
		inner := map[string]int{}
		for node, want := range perNode {
			have := 0
			if cur[rs] != nil {
				have = cur[rs][node]
			}
			out := 0
			if plannedOut[rs] != nil {
				out = plannedOut[rs][node]
			}
			r := want - have + out
			if r < 0 {
				r = 0
			}
			inner[node] = r
		}
		rem[rs] = inner
	}

	// 5) atomics
	counters := make(WorkloadNodeCounters, len(rem))
	for rs, byNode := range rem {
		inner := make(map[string]*atomic.Int32, len(byNode))
		for node, v := range byNode {
			ctr := new(atomic.Int32)
			ctr.Store(int32(v))
			inner[node] = ctr
		}
		counters[rs] = inner
	}

	// Cancel any previous plan
	if old := pl.getActive(); old != nil && old.Cancel != nil {
		old.Cancel()
	}

	ctxPlan, cancel := context.WithTimeout(context.Background(), PlanExecutionTTL)
	ap := &ActivePlanState{
		ID:        id,
		PlanDoc:   sp,
		Remaining: counters,
		Ctx:       ctxPlan,
		Cancel:    cancel,
	}
	pl.ActivePtr.Store(ap)

	// Start a watcher tied to this plan only
	go pl.watchPlanTimeout(ap)
}

func (pl *MyCrossNodePreemption) watchPlanTimeout(ap *ActivePlanState) {
	<-ap.Ctx.Done()
	// If Cancel() was called due to completion/replacement, do nothing.
	if ap.Ctx.Err() != context.DeadlineExceeded {
		return
	}
	// Ensure we're still looking at the same active plan
	cur := pl.getActive()
	if cur == nil || cur.ID != ap.ID || cur.PlanDoc.Completed {
		return
	}
	klog.InfoS("plan timeout reached; deactivating plan", "planID", ap.ID, "ttl", PlanExecutionTTL)
	// Mark completed for auditing, then settle
	pl.markPlanCompleted(context.Background(), ap.ID)
	pl.onPlanSettled()
}

func (pl *MyCrossNodePreemption) clearActive() {
	pl.ActivePtr.Store(nil)
}

func (pl *MyCrossNodePreemption) getActive() *ActivePlanState {
	return pl.ActivePtr.Load()
}

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(ctx context.Context) ([]v1.ConfigMap, error) {
	lst, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", ConfigMapLabelKey, "true"),
		},
	)
	if err != nil {
		return nil, err
	}
	sort.Slice(lst.Items, func(i, j int) bool {
		return lst.Items[i].CreationTimestamp.Time.After(lst.Items[j].CreationTimestamp.Time)
	})
	return lst.Items, nil
}

// markPlanCompleted sets Completed=true in json (i.e. not active plan).
func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || cm == nil {
			return nil
		}
		if err != nil {
			return err
		}
		raw := cm.Data["plan.json"]
		if raw == "" {
			return nil
		}
		var sp StoredPlan
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			klog.ErrorS(err, "markPlanCompleted: cannot decode plan.json", "configMap", cmName)
			return nil
		}
		if !sp.Completed {
			now := time.Now().UTC()
			sp.Completed = true
			sp.CompletedAt = &now
			b, _ := json.MarshalIndent(&sp, "", "  ")
			patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
			_, err = pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).
				Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		}
		return nil
	})

	if err := pl.pruneOldPlans(context.Background(), 20); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
}

func (pl *MyCrossNodePreemption) pruneOldPlans(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	items, err := pl.listPlans(ctx)
	if err != nil {
		return err
	}
	if len(items) <= keep {
		return nil
	}

	latestIncomplete := ""
	for i := range items {
		raw := items[i].Data["plan.json"]
		if raw == "" {
			continue
		}
		var sp StoredPlan
		if json.Unmarshal([]byte(raw), &sp) == nil && !sp.Completed {
			latestIncomplete = items[i].Name
			break
		}
	}

	keepSet := make(map[string]struct{}, keep)
	for i := 0; i < len(items) && len(keepSet) < keep; i++ {
		keepSet[items[i].Name] = struct{}{}
	}
	if latestIncomplete != "" {
		if _, ok := keepSet[latestIncomplete]; !ok {
			for i := keep - 1; i >= 0 && i < len(items); i-- {
				if _, ok := keepSet[items[i].Name]; ok && items[i].Name != latestIncomplete {
					delete(keepSet, items[i].Name)
					break
				}
			}
			keepSet[latestIncomplete] = struct{}{}
		}
	}

	for i := range items {
		name := items[i].Name
		if _, ok := keepSet[name]; ok {
			continue
		}
		if err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete old plan ConfigMap", "configMap", name)
		}
	}
	return nil
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, sp *StoredPlan) (bool, error) {
	// A) Pending/preemptor pod bound to target node
	if sp.TargetNode != "" && sp.PendingPod != "" {
		pns, pname := splitNamespaceName(sp.PendingPod)
		preemptor, err := pl.Client.CoreV1().Pods(pns).Get(ctx, pname, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get pending pod: %w", err)
		}
		if err == nil && preemptor.Spec.NodeName != sp.TargetNode {
			klog.V(2).InfoS("Plan incomplete: preemptor not yet bound",
				"pod", sp.PendingPod, "wantNode", sp.TargetNode, "haveNode", preemptor.Spec.NodeName)
			return false, nil
		}
	}

	// B) Standalone/name-addressed pods on specified node
	for name, node := range sp.PlacementsByName {
		ns := nsOf(sp.PendingPod)
		if strings.Contains(name, "/") {
			ns, name = splitNamespaceName(name)
		}
		pod, err := pl.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(2).InfoS("Plan incomplete: standalone pod missing",
					"pod", ns+"/"+name, "expectedNode", node)
				return false, nil
			}
			return false, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
		}
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName != node {
			klog.V(2).InfoS("Plan incomplete: standalone pod mismatch",
				"pod", ns+"/"+name, "expectedNode", node, "haveNode", pod.Spec.NodeName)
			return false, nil
		}
	}

	// C) Per-workload per-node quotas satisfied.
	for wkStr, perNode := range sp.WkDesiredPerNode {
		wk, ok := parseWorkloadKey(wkStr)
		if !ok {
			continue
		}
		lblSel, err := selectorForWorkload(ctx, pl.Client, wk)
		if err != nil {
			return false, fmt.Errorf("selector for %s: %w", wk.String(), err)
		}
		selector, err := metav1.LabelSelectorAsSelector(&lblSel)
		if err != nil {
			return false, fmt.Errorf("build selector for %s: %w", wk.String(), err)
		}

		podList, err := pl.Client.CoreV1().Pods(wk.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return false, fmt.Errorf("list pods for %s: %w", wk.String(), err)
		}

		counts := map[string]int{}
		for i := range podList.Items {
			p := &podList.Items[i]
			if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
				continue
			}
			// Skip the plan's preemptor: it is not part of WkDesiredPerNode by design.
			if sp.TargetNode != "" && string(p.UID) == sp.PendingUID {
				continue
			}
			if twk, ok := topWorkload(p); !ok || !workloadEqual(twk, wk) {
				continue
			}
			counts[p.Spec.NodeName]++
		}

		for node, want := range perNode {
			have := counts[node]
			if have != want {
				klog.V(2).InfoS("Plan incomplete: workload count mismatch",
					"workload", wk.String(), "node", node, "want", want, "have", have,
					"note", "counts exclude preemptor")
				return false, nil
			}
		}
	}

	return true, nil
}

// derivePlanDocs computes PlacementsByName (standalone pods) and WkDesiredPerNode (per-RS/node quotas)
// from the solver placements and current live pods (+ pending).
func (pl *MyCrossNodePreemption) derivePlanDocs(
	out *SolverOutput,
	pending *v1.Pod, // may be nil in batch
) (map[string]string, map[string]map[string]int, error) {

	// UID -> *v1.Pod
	podsByUID := map[string]*v1.Pod{}
	live, err := pl.getPods()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range live {
		podsByUID[string(p.UID)] = p
	}
	if pending != nil {
		podsByUID[string(pending.UID)] = pending
	}

	byName := make(map[string]string)
	rsDesired := map[string]map[string]int{}

	for uid, node := range out.Placements {
		p := podsByUID[uid]
		if p == nil {
			continue
		}

		// In single-preemptor mode: skip allocating the preemptor via RS quotas if NominatedNode is set
		isLeadPreemptor := pending != nil && out != nil && out.NominatedNode != "" && uid == string(pending.UID)
		if isLeadPreemptor {
			continue
		}

		if wk, ok := topWorkload(p); ok {
			key := wk.String()
			if _, ok := rsDesired[key]; !ok {
				rsDesired[key] = map[string]int{}
			}
			rsDesired[key][node]++
		} else {
			byName[p.Namespace+"/"+p.Name] = node
		}
	}
	return byName, rsDesired, nil
}

func toSolverPod(p *v1.Pod, where string) SolverPod {
	return SolverPod{
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Name:      p.Name,
		CPU_m:     getPodCPURequest(p),
		MemBytes:  getPodMemoryRequest(p),
		Priority:  getPodPriority(p),
		Where:     where,
	}
}

// ---------------------------- Plan translation / export / execution -----------

func (pl *MyCrossNodePreemption) translatePlanFromSolver(
	out *SolverOutput,
	pending *v1.Pod, // may be nil
) (*Plan, error) {

	plan := &Plan{TargetNode: out.NominatedNode}

	// Build quick UID -> *v1.Pod map (live + pending)
	podsByUID := map[string]*v1.Pod{}
	live, err := pl.getPods()
	if err != nil {
		return nil, err
	}
	for _, p := range live {
		podsByUID[string(p.UID)] = p
	}
	if pending != nil {
		podsByUID[string(pending.UID)] = pending
	}

	// Evictions
	for _, e := range out.Evictions {
		if p, ok := podsByUID[e.UID]; ok {
			plan.Evicts = append(plan.Evicts, Evict{
				Pod:      PodRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)},
				FromNode: p.Spec.NodeName,
				CPUm:     getPodCPURequest(p),
				MemBytes: getPodMemoryRequest(p),
			})
		}
	}

	// Moves (only for running pods assigned a different node)
	for uid, dest := range out.Placements {
		// Skip preemptor only when we actually have one
		if pending != nil && uid == string(pending.UID) {
			continue
		}
		p, ok := podsByUID[uid]
		if !ok {
			continue
		}
		from := p.Spec.NodeName
		if from == "" || dest == "" || from == dest {
			continue
		}
		plan.Moves = append(plan.Moves, Move{
			Pod:      PodRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)},
			FromNode: from,
			ToNode:   dest,
			CPUm:     getPodCPURequest(p),
			MemBytes: getPodMemoryRequest(p),
		})
	}

	return plan, nil
}

func (pl *MyCrossNodePreemption) exportPlanToConfigMap(
	ctx context.Context,
	plan *Plan,
	out *SolverOutput,
	pending *v1.Pod, // may be nil
) (string, error) {

	byName, rsDesired, err := pl.derivePlanDocs(out, pending)
	if err != nil {
		return "", err
	}

	doc := &StoredPlan{
		Completed:        false,
		CompletedAt:      nil,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		SolverOutput:     out,
		Plan:             *plan,
		PlacementsByName: byName,
		WkDesiredPerNode: rsDesired,
	}

	// Fill single-preemptor metadata only when provided
	if pending != nil {
		doc.PendingPod = fmt.Sprintf("%s/%s", pending.Namespace, pending.Name)
		doc.PendingUID = string(pending.UID)
		doc.TargetNode = plan.TargetNode // may be empty, kept for symmetry
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	// Make a unique name
	name := fmt.Sprintf("crossnode-plan-%d", time.Now().UnixNano())
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ConfigMapNamespace,
			Labels:    map[string]string{ConfigMapLabelKey: "true"},
		},
		Data: map[string]string{"plan.json": string(raw)},
	}
	if _, err := pl.Client.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return "", err
	}

	_ = pl.pruneOldPlans(ctx, 20)
	return name, nil
}

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *Plan) error {
	ap := pl.getActive()
	ctxPlan := ctx
	if ap != nil && ap.Ctx != nil {
		ctxPlan = ap.Ctx
	}

	// Resolve pods for all ops upfront (by UID) so we can evict/recreate correctly.
	lister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()

	resolve := func(uid, ns, name string) *v1.Pod {
		// 1) Fast path: informer lister by name, then verify UID
		if p, err := lister.Pods(ns).Get(name); err == nil && p != nil && string(p.UID) == uid {
			return p
		}
		// 2) Fallback: list namespace from lister and match by UID (handles name reuse)
		if pods, err := lister.Pods(ns).List(labels.Everything()); err == nil {
			for _, p := range pods {
				if string(p.UID) == uid {
					return p
				}
			}
		}
		// 3) Last resort: direct GET by name and check UID
		if p, err := pl.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err == nil && p != nil && string(p.UID) == uid {
			return p
		}
		return nil
	}

	var targets []*v1.Pod
	seenUID := make(map[string]bool, len(plan.Moves)+len(plan.Evicts))

	addTarget := func(p *v1.Pod) {
		if p == nil {
			return
		}
		uid := string(p.UID)
		if !seenUID[uid] {
			seenUID[uid] = true
			targets = append(targets, p)
		}
	}

	for _, mv := range plan.Moves {
		addTarget(resolve(mv.Pod.UID, mv.Pod.Namespace, mv.Pod.Name))
	}
	for _, e := range plan.Evicts {
		addTarget(resolve(e.Pod.UID, e.Pod.Namespace, e.Pod.Name))
	}

	for _, mv := range plan.Moves {
		klog.V(2).InfoS("Pod movement planned",
			"pod", mv.Pod.Namespace+"/"+mv.Pod.Name,
			"from", mv.FromNode, "to", mv.ToNode,
			"cpu(m)", mv.CPUm, "mem(MiB)", bytesToMiB(mv.MemBytes),
		)
	}
	for _, e := range plan.Evicts {
		klog.V(2).InfoS("Eviction planned",
			"pod", e.Pod.Namespace+"/"+e.Pod.Name,
			"from", e.FromNode,
			"cpu(m)", e.CPUm, "mem(MiB)", bytesToMiB(e.MemBytes),
		)
	}

	// 1) Evict all targeted pods and wait for them to disappear.
	if len(targets) > 0 {
		klog.V(2).InfoS("Evicting/awaiting eviction of targeted pods", "count", len(targets))
		for _, p := range targets {
			if err := pl.evictPod(ctxPlan, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict pod %s: %w", podRef(p), err)
			}
		}
		if err := pl.waitPodsGone(ctxPlan, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// 2) Recreate standalone pods (controllers will recreate RS/SS/Job pods).
	for _, p := range targets {
		if _, owned := topWorkload(p); owned {
			continue
		}
		klog.V(2).InfoS("Recreating standalone pod (no bind)", "pod", podRef(p))
		if err := pl.recreatePod(ctxPlan, p, ""); err != nil {
			return fmt.Errorf("recreate standalone pod %s: %w", podRef(p), err)
		}
	}

	return nil
}

func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	// Use context's deadline if any, else default to 2m
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}
		for k := range remaining {
			got, err := pl.Client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				delete(remaining, k)
				continue
			}
			if err != nil {
				// transient error: keep polling
				return false, nil
			}
			if string(got.UID) != k.uid {
				// name reused; original is gone
				delete(remaining, k)
			}
		}
		if len(remaining) == 0 {
			return true, nil
		}
		klog.V(2).InfoS("Waiting for targeted pods to disappear", "remaining", len(remaining))
		return false, nil
	})
}

func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.NodeName = "" // no direct binding
	newp.Spec.NodeSelector = map[string]string{}

	if _, err := pl.Client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	return nil
}

func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		},
	}
	return pl.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
}

func (pl *MyCrossNodePreemption) onPlanSettled() bool {
	// Just double-check plan is not already completed
	ap := pl.getActive()
	if ap == nil || ap.PlanDoc.Completed {
		return false
	}
	klog.InfoS("plan settled; deactivating active plan", "planID", ap.ID)
	if ap.Cancel != nil {
		ap.Cancel() // stop the timeout watcher
	}
	pl.markPlanCompleted(context.Background(), ap.ID)
	pl.clearActive()
	pl.activateBlockedPods()
	return true
}

func (pl *MyCrossNodePreemption) publishPlan(
	ctx context.Context,
	out *SolverOutput,
	pending *v1.Pod, // may be nil
) (*Plan, *ActivePlanState, error) {

	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil {
		return nil, nil, fmt.Errorf("plan translation failed: %w", err)
	}

	// Export plan for auditing (CM id also becomes our plan ID)
	cmName, err := pl.exportPlanToConfigMap(ctx, plan, out, pending)
	if err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	// Derive docs (placementsByName + per-workload quotas)
	byName, wkDesired, derr := pl.derivePlanDocs(out, pending)
	if derr != nil {
		klog.ErrorS(derr, "derive plan docs failed (non-fatal)")
	}

	inMem := &StoredPlan{
		Completed:        false,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		SolverOutput:     out,
		Plan:             *plan,
		PlacementsByName: byName,
		WkDesiredPerNode: wkDesired,
	}

	// Only fill these when we actually have a single-preemptor
	if pending != nil {
		inMem.PendingPod = fmt.Sprintf("%s/%s", pending.Namespace, pending.Name)
		inMem.PendingUID = string(pending.UID)
		inMem.TargetNode = plan.TargetNode
	}

	pl.setActivePlan(inMem, cmName)
	return plan, pl.getActive(), nil
}
