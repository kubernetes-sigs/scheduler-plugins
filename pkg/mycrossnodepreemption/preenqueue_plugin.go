// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreEnqueue is called before a pod is enqueued for scheduling.
func (pl *MyCrossNodePreemption) PreEnqueue(ctx context.Context, pod *v1.Pod) *framework.Status {
	// Always allow kube-system pods
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}

	if !pl.CachesWarm.Load() {
		pl.Blocked.AddPod(pod)
		klog.V(V2).Info("Caches not warmed up yet; skipping plugin logic")
		return framework.NewStatus(framework.Pending, "Caches not warmed up yet; skipping plugin logic")
	}
	_ = pl.pruneSetEntries(pl.Blocked)

	switch pl.decideStrategy(PhasePreEnqueue) {
	case DecidePassThrough:
		klog.V(V2).InfoS("PreEnqueue: pass-through", "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)

	case DecideBatch:
		klog.V(V2).InfoS("PreEnqueue: batched pod", "pod", klog.KObj(pod))
		pl.Batched.AddPod(pod)
		return framework.NewStatus(framework.Pending, "PreEnqueue: batched pod")

	case DecideBlockActive:
		if !pl.allowedByActivePlan(pod) {
			klog.V(V2).InfoS("PreEnqueue: active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddPod(pod)
			return framework.NewStatus(framework.Pending, "PreEnqueue: active plan; blocking")
		}
		return framework.NewStatus(framework.Success) // fallback

	case DecideEvery:
		klog.InfoS("PreEnqueue: start", "pod", klog.KObj(pod))
		_, err := pl.runFlow(ctx, PhasePreEnqueue, pod)
		if err != nil {
			if err == ErrActiveInProgress {
				pl.Blocked.AddPod(pod)
				return framework.NewStatus(framework.Pending, "PreEnqueue: active plan in progress")
			}
			if err == ErrSolver {
				pl.Blocked.AddPod(pod)
				return framework.NewStatus(framework.Pending, "PreEnqueue: solver failed")
			}
			// else ErrRegisterPlan
			return framework.NewStatus(framework.Pending, "PreEnqueue: register plan failed")
		}
		return framework.NewStatus(framework.Success, "PreEnqueue: nominated after plan execution")
	}

	return framework.NewStatus(framework.Success)
}
