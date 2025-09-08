// plan_execution.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// TODO:
func (pl *MyCrossNodePreemption) registerPlan(
	ctx context.Context,
	out *SolverOutput,
	summary SolverSummary,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*StoredPlan, *ActivePlanState, string, error) {

	evicts, moves, oldPlc, newPlc, nominated, err := pl.buildActionsFromSolver(out, preemptor, pods)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build actions: %w", err)
	}

	doc := &StoredPlan{
		PluginVersion:   Version,
		Mode:            modeToString(),
		GeneratedAt:     time.Now().UTC(),
		Status:          PlanStatusActive,
		Evicts:          evicts,
		Moves:           moves,
		Solver:          summary,
		OldPlacements:   oldPlc,
		PlacementByName: newPlc, // includes both pending and moved (also preemptor) - also replica-pods
	}

	doc.WorkloadQuotasDoc = computeWorkloadQuotasFromPlan(doc, pods) // use the same live snapshot you pass around
	if len(doc.WorkloadQuotasDoc) == 0 {
		doc.WorkloadQuotasDoc = nil // omit empty in JSON
	}

	if preemptor != nil {
		// TODO: Get target node from placements
		doc.Preemptor = &Preemtor{
			Pod: Pod{
				UID:       string(preemptor.UID),
				Namespace: preemptor.Namespace,
				Name:      preemptor.Name,
			},
			NominatedNode: nominated,
		}
	}

	// Unique plan id (and ConfigMap name)
	id := fmt.Sprintf("crossnode-plan-%d", time.Now().UnixNano())

	pl.setActivePlan(doc, id, pods)

	// Export (JSON) to ConfigMap for audit
	if err := pl.exportPlanToConfigMap(ctx, id, doc); err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	return doc, pl.getActivePlan(), nominated, nil
}

// TODO
// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, sp *StoredPlan) error {
	ap := pl.getActivePlan()
	ctxPlan := ctx
	if ap != nil && ap.Ctx != nil {
		ctxPlan = ap.Ctx
	}

	resolve := func(uid, ns, name string) *v1.Pod {
		l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
		if p, err := l.Pods(ns).Get(name); err == nil && p != nil && string(p.UID) == uid {
			return p
		}
		if pods, err := l.Pods(ns).List(labels.Everything()); err == nil {
			for _, p := range pods {
				if string(p.UID) == uid {
					return p
				}
			}
		}
		return nil
	}

	// Build unique target set = (moves + evicts)
	seen := map[string]bool{}
	var targets []*v1.Pod
	add := func(uid, ns, name string) {
		if seen[uid] {
			return
		}
		seen[uid] = true
		if p := resolve(uid, ns, name); p != nil {
			targets = append(targets, p)
		}
	}
	for _, mv := range sp.Moves {
		add(mv.Pod.UID, mv.Pod.Namespace, mv.Pod.Name)
	}
	for _, e := range sp.Evicts {
		add(e.Pod.UID, e.Pod.Namespace, e.Pod.Name)
	}

	for _, mv := range sp.Moves {
		klog.V(V2).InfoS("Pod movement planned",
			"pod", mv.Pod.Namespace+"/"+mv.Pod.Name, "from", mv.FromNode, "to", mv.ToNode)
	}
	for _, e := range sp.Evicts {
		klog.V(V2).InfoS("Eviction planned",
			"pod", e.Pod.Namespace+"/"+e.Pod.Name, "from", e.Node)
	}

	if len(targets) > 0 {
		klog.V(V2).InfoS("Evicting/awaiting targeted pods", "count", len(targets))
		for _, p := range targets {
			if err := pl.evictPod(ctxPlan, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("evict pod %s: %w", podRef(p), err)
			}
		}
		if err := pl.waitPodsGone(ctxPlan, targets); err != nil {
			return fmt.Errorf("wait for targeted pods gone: %w", err)
		}
	}
	// Recreate standalone pods only
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
