// plan_activation.go
package mypriorityoptimizer

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// planActivation activates all live pending pods that the plan intends to place
// (i.e., NewPlacement with FromNode == "" and ToNode != "").
func (pl *SharedState) planActivation(plan *Plan, pods []*v1.Pod) error {
	// If no plan, nothing to do
	if plan == nil {
		klog.V(MyV).ErrorS(ErrNoPlanProvided, InfoNoPlanProvided, nil)
		return ErrNoPlanProvided
	}

	// Resolve active-plan context (for timeouts)
	ap := pl.getActivePlan()
	var baseCtx context.Context
	if ap != nil && ap.Ctx != nil {
		baseCtx = context.WithoutCancel(ap.Ctx)
	} else {
		baseCtx = context.Background()
	}
	overallCtx, cancel := context.WithTimeout(baseCtx, PlanOverallTimeout)
	defer cancel()

	// 1) Resolve unique targets (pods that are moved or evicted)
	seen := map[types.UID]bool{}
	var targets []*v1.Pod
	add := func(uid types.UID, ns, name string) {
		if seen[uid] {
			return
		}
		seen[uid] = true
		if pod := pl.getPod(uid, ns, name); pod != nil {
			targets = append(targets, pod)
		}
	}
	for _, mv := range plan.Moves {
		add(mv.Pod.UID, mv.Pod.Namespace, mv.Pod.Name)
	}
	for _, e := range plan.Evicts {
		add(e.Pod.UID, e.Pod.Namespace, e.Pod.Name)
	}

	// 2) Log plan details and evict (if any)
	if len(plan.Moves) == 0 && len(plan.Evicts) == 0 {
		klog.V(MyV).Info("plan has no moves or evictions")
	} else {
		for _, mv := range plan.Moves {
			klog.V(MyV).InfoS("pod movement planned",
				"pod", mergeNsName(mv.Pod.Namespace, mv.Pod.Name),
				"from", mv.FromNode, "to", mv.ToNode,
			)
		}
		for _, e := range plan.Evicts {
			klog.V(MyV).InfoS("eviction planned",
				"pod", mergeNsName(e.Pod.Namespace, e.Pod.Name),
				"from", e.Node,
			)
		}

		if len(targets) > 0 {
			klog.V(MyV).InfoS("executePlan: evicting targeted pods", "count", len(targets))
			if err := pl.evictTargets(overallCtx, targets); err != nil {
				return err
			}
			waitCtx, cancel := context.WithTimeout(overallCtx, WaitPodsGoneTimeout)
			defer cancel()
			if err := pl.waitPodsGone(waitCtx, targets); err != nil {
				return fmt.Errorf("wait for targeted pods gone: %w", err)
			}
		}
	}

	// 3) Activate planned pending
	pl.activatePlannedPending(plan, pods)

	return nil

}
