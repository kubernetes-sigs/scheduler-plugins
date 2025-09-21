// plan.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// TODO: Reach to here in this file...

// registerPlan builds and registers a new plan as active, exporting it to a ConfigMap.
func (pl *MyCrossNodePreemption) registerPlan(
	ctx context.Context,
	out *SolverOutput,
	summary SolverSummary,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*StoredPlan, *ActivePlan, string, error) {

	evicts, moves, oldPlacement, newPlacement, placementByName, workloadQuotas, nominated, err := pl.buildPlan(out, preemptor, pods)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build actions: %w", err)
	}

	doc := &StoredPlan{
		PluginVersion:        Version,
		OptimizationStrategy: strategyToString(),
		GeneratedAt:          time.Now().UTC(),
		Status:               PlanStatusActive,
		Evicts:               evicts,
		Moves:                moves,
		Solver:               summary,
		OldPlacements:        oldPlacement,
		NewPlacement:         newPlacement,
		PlacementByName:      placementByName,
		WorkloadQuotasDoc:    workloadQuotas,
	}

	if preemptor != nil {
		doc.Preemptor = &Preemptor{
			Pod: Pod{
				UID:       preemptor.UID,
				Namespace: preemptor.Namespace,
				Name:      preemptor.Name,
			},
			NominatedNode: nominated,
		}
	}

	// Unique plan id (and ConfigMap name)
	id := fmt.Sprintf("plan-%d", time.Now().UnixNano())

	pl.setActivePlan(doc, id, pods)

	// Export (JSON) to ConfigMap for audit
	if err := pl.exportPlanToConfigMap(ctx, id, doc); err != nil {
		klog.ErrorS(err, "export plan failed (non-fatal)")
	}

	return doc, pl.getActivePlan(), nominated, nil
}

// executePlan executes the given plan: evicting and recreating pods as needed.
func (pl *MyCrossNodePreemption) executePlan(sp *StoredPlan) error {
	ap := pl.getActivePlan()

	// Base context for I/O: keep any deadline, but ignore plan cancellation.
	var base context.Context
	if ap != nil && ap.Ctx != nil {
		base = context.WithoutCancel(ap.Ctx)
	} else {
		base = context.Background()
	}
	// Optional overall cap for this whole method.
	overallCtx, overallCancel := context.WithTimeout(base, 5*time.Minute)
	defer overallCancel()

	resolve := func(uid types.UID, ns, name string) *v1.Pod {
		podsLister := pl.podsLister()
		if p, err := podsLister.Pods(ns).Get(name); err == nil && p != nil && p.UID == uid {
			return p
		}
		if pods, err := podsLister.Pods(ns).List(labels.Everything()); err == nil {
			for _, p := range pods {
				if p.UID == uid {
					return p
				}
			}
		}
		return nil
	}

	// Build unique target set = (moves + evicts)
	seen := map[types.UID]bool{}
	var targets []*v1.Pod
	add := func(uid types.UID, ns, name string) {
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
		klog.V(MyV).InfoS("Pod movement planned",
			"pod", mv.Pod.Namespace+"/"+mv.Pod.Name, "from", mv.FromNode, "to", mv.ToNode)
	}
	for _, e := range sp.Evicts {
		klog.V(MyV).InfoS("Eviction planned",
			"pod", e.Pod.Namespace+"/"+e.Pod.Name, "from", e.Node)
	}

	if len(targets) > 0 {
		klog.V(MyV).InfoS("Evicting/awaiting targeted pods", "count", len(targets))

		// 1) Evict with bounded parallelism + per-op timeout
		{
			g, gctx := errgroup.WithContext(overallCtx)
			g.SetLimit(8) // tune as needed
			for _, p := range targets {
				p := p
				g.Go(func() error {
					opCtx, cancel := context.WithTimeout(gctx, 30*time.Second)
					defer cancel()
					if err := pl.evictPod(opCtx, p); err != nil && !apierrors.IsNotFound(err) {
						return fmt.Errorf("evict %s: %w", podRef(p), err)
					}
					return nil
				})
			}
			if err := g.Wait(); err != nil {
				return err
			}
		}

		// 2) Wait for the evicted pods to actually disappear from cache (own timeout)
		{
			waitCtx, cancel := context.WithTimeout(overallCtx, 1*time.Minute)
			defer cancel()
			if err := pl.waitPodsGone(waitCtx, targets); err != nil {
				return fmt.Errorf("wait for targeted pods gone: %w", err)
			}
		}
	}

	// 3) Recreate standalone pods only (controllers will recreate theirs)
	{
		g, gctx := errgroup.WithContext(overallCtx)
		g.SetLimit(8) // number of concurrent creations
		for _, p := range targets {
			if _, owned := topWorkload(p); owned {
				continue
			}
			p := p
			g.Go(func() error {
				opCtx, cancel := context.WithTimeout(gctx, 30*time.Second)
				defer cancel()
				klog.V(MyV).InfoS("Recreating standalone pod (no bind)", "pod", podRef(p))
				if err := pl.recreatePod(opCtx, p, ""); err != nil {
					return fmt.Errorf("recreate %s: %w", podRef(p), err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}

	return nil
}
