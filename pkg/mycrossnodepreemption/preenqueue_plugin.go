// preenqueue_plugin.go (refactored)
package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (pl *MyCrossNodePreemption) PreEnqueue(ctx context.Context, pod *v1.Pod) *framework.Status {

	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}
	_ = pl.pruneStaleSetEntries(pl.Blocked)

	switch pl.decideStrategy(PhasePreEnqueue) {
	case DecidePassThrough:
		klog.V(V2).InfoS("PreEnqueue: pass-through", "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)

	case DecideBatch:
		klog.V(V2).InfoS("PreEnqueue: batched pod", "pod", klog.KObj(pod))
		pl.Batched.AddPod(pod)
		return framework.NewStatus(framework.Pending, "PreEnqueue: batched pod")

	case DecideBlockActive:
		if pl.Active.Load() && !pl.allowedByActivePlan(pod) {
			klog.V(V2).InfoS("PreEnqueue: active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddPod(pod)
			return framework.NewStatus(framework.Pending, "PreEnqueue: active plan; blocking")
		}
		return framework.NewStatus(framework.Success) // fallback

	case DecideEvery:
		klog.InfoS("PreEnqueue: start", "pod", klog.KObj(pod))
		_, err := pl.runSingleFlow(ctx, pod, PhasePreEnqueue)
		if err != nil {
			if err == ErrActiveInProgress {
				pl.Blocked.AddPod(pod)
				klog.V(V2).InfoS("PreEnqueue: active plan in progress", "pod", klog.KObj(pod))
				return framework.NewStatus(framework.Pending, "PreEnqueue: active plan in progress")
			}
			if err == ErrNoNomination {
				pl.Blocked.AddPod(pod)
				klog.InfoS("PreEnqueue: no nomination", "pod", klog.KObj(pod))
				return framework.NewStatus(framework.Pending, "PreEnqueue: solver found no solution")
			}
			klog.ErrorS(err, "PreEnqueue: plan failed", "pod", klog.KObj(pod))
			return framework.NewStatus(framework.Pending, "PreEnqueue: plan failed")
		}
		return framework.NewStatus(framework.Success, "PreEnqueue: nominated after plan execution")
	}

	return framework.NewStatus(framework.Success)
}
