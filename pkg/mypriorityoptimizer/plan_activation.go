// plan_activation.go
package mypriorityoptimizer

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Small indirections to make planActivation easier to test.
var activatePlannedPendingFn = func(pl *SharedState, plan *Plan, pods []*v1.Pod) {
	pl.activatePlannedPending(plan, pods)
}

var getPodForPlanActivation = func(pl *SharedState, uid types.UID, ns, name string) *v1.Pod {
	return pl.getPod(uid, ns, name)
}

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
		if pod := getPodForPlanActivation(pl, uid, ns, name); pod != nil {
			targets = append(targets, pod)
		}
	}
	for _, mv := range plan.Moves {
		add(mv.UID, mv.Namespace, mv.Name)
	}
	for _, e := range plan.Evicts {
		add(e.UID, e.Namespace, e.Name)
	}

	// 2) Log plan details and evict (if any)
	if len(plan.Moves) == 0 && len(plan.Evicts) == 0 {
		klog.V(MyV).Info("plan has no moves or evictions")
	} else {
		for _, mv := range plan.Moves {
			klog.V(MyV).InfoS("pod movement planned",
				"pod", mergeNsName(mv.Namespace, mv.Name),
				"from", mv.OldNode, "to", mv.Node,
			)
		}
		for _, e := range plan.Evicts {
			klog.V(MyV).InfoS("eviction planned",
				"pod", mergeNsName(e.Namespace, e.Name),
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
	activatePlannedPendingFn(pl, plan, pods)

	return nil
}
