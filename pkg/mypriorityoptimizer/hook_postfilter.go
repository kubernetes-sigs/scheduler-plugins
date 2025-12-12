// hook_postfilter.go
package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var postFilterSleep = time.Sleep

// injectable (test seams)
var postFilterPerPodEnabled = isPerPodMode
var postFilterRunOptimization = func(pl *SharedState, ctx context.Context, pending *v1.Pod) (*Plan, error) {
	plan, _, _, _, _, err := pl.runOptimizationFlow(ctx, pending)
	return plan, err
}

// PostFilter is called if no nodes are found to run the Pod in the filtering phase.
// CHECKED
func (pl *SharedState) PostFilter(ctx context.Context, state fwk.CycleState, pending *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *fwk.Status) {
	stage := "PostFilter"

	// Only proceed if PerPod is enabled; otherwise, just skip.
	if !postFilterPerPodEnabled() {
		klog.V(MyV).InfoS(msg(stage, "no nomination"), "pod", klog.KObj(pending))
		return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, "no nomination"))
	}

	// If active plan in progress, block the pod.
	ap := pl.getActivePlan()
	if ap != nil {
		klog.V(MyV).InfoS(msg(stage, InfoActivePlanInProgress+"; "+InfoBlockPod), "pod", klog.KObj(pending))
		pl.BlockedWhileActive.AddPodSafely(pending)
		return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoActivePlanInProgress))
	}

	klog.InfoS(msg(stage, "start"), "pod", klog.KObj(pending))
	postFilterSleep(1 * time.Second)

	plan, err := postFilterRunOptimization(pl, ctx, pending)
	if err != nil {
		switch err {
		case ErrActiveInProgress:
			pl.BlockedWhileActive.AddPodSafely(pending)
			return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoActivePlanInProgress))
		default:
			return nil, fwk.NewStatus(fwk.Unschedulable, msg(stage, InfoPlanRegistrationFailed))
		}
	}

	return &framework.PostFilterResult{
		NominatingInfo: &framework.NominatingInfo{
			NominatedNodeName: plan.NominatedNode,
			NominatingMode:    framework.ModeOverride,
		},
	}, fwk.NewStatus(fwk.Success, msg(stage, InfoNominatedAfterPlan))
}
