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
func (pl *MyCrossNodePreemption) registerPlan(ctx context.Context, out *SolverOutput, summary SolverSummary, preemptor *v1.Pod) (*StoredPlan, *ActivePlanState, string, error) {

	evicts, moves, newPls, nominatedNode, err := pl.buildActionsFromSolver(out, preemptor)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build actions: %w", err)
	}

	oldPls, err := pl.snapshotOldPlacements()
	if err != nil {
		return nil, nil, "", fmt.Errorf("snapshot old placements: %w", err)
	}

	wkDesired := pl.deriveWorkloadPerNode(newPls, preemptor)

	doc := &StoredPlan{
		PluginVersion:   Version,
		Mode:            modeToString(),
		GeneratedAt:     time.Now().UTC(),
		Status:          PlanStatusActive,
		Evicts:          evicts,
		Moves:           moves,
		Solver:          summary,
		OldPlacements:   oldPls,
		NewPlacements:   newPls, // includes both pending and moved (also preemptor)
		WorkloadPerNode: wkDesired,
	}

	if preemptor != nil {
		// TODO: Get target node from placements
		doc.Preemptor = &Preemtor{
			Pod: PodLite{
				UID:       string(preemptor.UID),
				Namespace: preemptor.Namespace,
				Name:      preemptor.Name,
			},
			NominatedNode: nominatedNode,
		}
	}

	// Unique plan id (and ConfigMap name)
	id := fmt.Sprintf("crossnode-plan-%d", time.Now().UnixNano())

	pl.setActivePlan(doc, id)

	// Export (JSON) to ConfigMap for audit
	if err := pl.exportPlanToConfigMap(ctx, id, doc); err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	return doc, pl.getActivePlan(), nominatedNode, nil
}

// deriveWorkloadPerNode derives the desired workload distribution per node from the new placements.
// It also includes a preemptor-pod if exists.
func (pl *MyCrossNodePreemption) deriveWorkloadPerNode(newPlacements []NewPlacements, preemptor *v1.Pod) WorkloadPerNode {
	wkDesired := WorkloadPerNode{}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()

	for _, plm := range newPlacements {
		p, err := podLister.Pods(plm.Pod.Namespace).Get(plm.Pod.Name)
		if err != nil || p == nil {
			continue
		}
		if preemptor != nil && p.UID == preemptor.UID {
			// Don't include the preemptor in the workload; will cause troubles
			// since another pod from the same replicaset could takes it place.
			continue
		}
		if wk, ok := topWorkload(p); ok { // only consider pods from workloads
			key := wk.String()
			m, ok := wkDesired[key]
			if !ok {
				m = map[string]int{}
				wkDesired[key] = m
			}
			m[plm.TargetNode]++
		}
	}
	return wkDesired
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
			"pod", e.Pod.Namespace+"/"+e.Pod.Name, "from", e.FromNode)
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
