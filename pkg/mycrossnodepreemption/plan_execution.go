// plan_execution.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) registerPlan(
	ctx context.Context,
	out *SolverOutput,
	pending *v1.Pod, // may be nil
) (*Plan, *ActivePlanState, error) {

	plan, err := pl.translatePlanFromSolver(out, pending)
	if err != nil {
		return nil, nil, fmt.Errorf("plan translation failed: %w", err)
	}

	// Derive docs (placementsByName + per-workload quotas)
	placementsByName, workloadDesired, err := pl.derivePlan(out, pending)
	if err != nil {
		klog.ErrorS(err, "derive plan docs failed (non-fatal)")
	}

	// Export plan for auditing (CM id also becomes our plan ID)
	name := fmt.Sprintf("crossnode-plan-%d", time.Now().UnixNano())

	inMem := &StoredPlan{
		Completed:        false,
		GeneratedAt:      time.Now().UTC(),
		PluginVersion:    Version,
		Mode:             modeToString(),
		SolverOutput:     out,
		Plan:             *plan,
		PlacementsByName: placementsByName,
		WkDesiredPerNode: workloadDesired,
	}

	// Only fill these when we actually have a single-preemptor
	if pending != nil {
		inMem.PendingPod = fmt.Sprintf("%s/%s", pending.Namespace, pending.Name)
		inMem.PendingUID = string(pending.UID)
		inMem.TargetNode = plan.TargetNode
	}

	pl.setActivePlan(inMem, name)

	err = pl.exportPlanToConfigMap(ctx, name, plan, out, pending, placementsByName, workloadDesired)
	if err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	return plan, pl.getActivePlan(), nil
}

// derivePlan computes PlacementsByName (standalone pods) and WkDesiredPerNode (per-RS/node quotas)
// from the solver placements and current live pods (+ pending).
func (pl *MyCrossNodePreemption) derivePlan(
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

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *Plan) error {
	ap := pl.getActivePlan()
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
		klog.V(V2).InfoS("Pod movement planned",
			"pod", mv.Pod.Namespace+"/"+mv.Pod.Name,
			"from", mv.FromNode, "to", mv.ToNode,
			"cpu(m)", mv.CPUm, "mem(MiB)", bytesToMiB(mv.MemBytes),
		)
	}
	for _, e := range plan.Evicts {
		klog.V(V2).InfoS("Eviction planned",
			"pod", e.Pod.Namespace+"/"+e.Pod.Name,
			"from", e.FromNode,
			"cpu(m)", e.CPUm, "mem(MiB)", bytesToMiB(e.MemBytes),
		)
	}

	// Evict all targeted pods and wait for them to disappear.
	if len(targets) > 0 {
		klog.V(V2).InfoS("Evicting/awaiting eviction of targeted pods", "count", len(targets))
		for _, p := range targets {
			if err := pl.evictPod(ctxPlan, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict pod %s: %w", podRef(p), err)
			}
		}
		if err := pl.waitPodsGone(ctxPlan, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}

	// Recreate standalone pods (controllers will recreate RS/SS/Job pods).
	for _, p := range targets {
		if _, owned := topWorkload(p); owned {
			continue
		}
		klog.V(V2).InfoS("Recreating standalone pod (no bind)", "pod", podRef(p))
		if err := pl.recreatePod(ctxPlan, p, ""); err != nil {
			return fmt.Errorf("recreate standalone pod %s: %w", podRef(p), err)
		}
	}

	return nil
}
