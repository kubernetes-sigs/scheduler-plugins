// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreEnqueuePlugin = &MyCrossNodePreemption{}

// PreEnqueue is called before a pod is enqueued (first point of contact).
// It is used, here, to determine if a pod can be enqueued or if it should be blocked/batched according to the active plan.
func (pl *MyCrossNodePreemption) PreEnqueue(ctx context.Context, pod *v1.Pod) *framework.Status {
	_ = pl.pruneStaleSetEntries(pl.Blocked)

	// Always allow kube-system to proceed.
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}

	if optimizeForEvery() && optimizeAtPreEnqueue() {
		if !pl.tryEnterActive() {
			klog.V(V2).InfoS("PreEnqueue: every-preemptor pod", "pod", klog.KObj(pod))
			pl.Blocked.AddPod(pod)
			return framework.NewStatus(framework.Pending, "PreEnqueue: every-preemptor pod")
		}
		klog.InfoS("PreEnqueue: start", "pod", klog.KObj(pod))
		ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
		defer cancel()

		startTime := time.Now()
		out, err := pl.solve(ctxSolve, SolveSingle, pod, nil, SolverTimeout)
		solverDuration := time.Since(startTime)
		if err != nil || out.NominatedNode == "" {
			klog.ErrorS(err, "PreEnqueue: solver found no solution", "pod", klog.KObj(pod), "duration", time.Since(startTime))
			pl.leaveActive()
			return framework.NewStatus(framework.Pending, "PreEnqueue: solver found no solution")
		}

		plan, ap, err := pl.registerPlan(ctx, out, pod)
		if err != nil {
			klog.ErrorS(err, "PreEnqueue: plan registration failed", "pod", klog.KObj(pod))
			pl.Blocked.AddPod(pod)
			pl.leaveActive()
			return framework.NewStatus(framework.Pending, err.Error())
		}

		// Skip plan execution if no moves or evictions
		if len(plan.Moves) > 0 || len(plan.Evicts) > 0 {
			if err := pl.executePlan(ctx, plan); err != nil {
				klog.ErrorS(err, "PreEnqueue: plan execution failed")
				pl.Blocked.AddPod(pod)
				pl.onPlanSettled()
				return framework.NewStatus(framework.Pending, "PreEnqueue: plan execution failed")
			}
		}

		klog.InfoS("PreEnqueue: plan execution finished",
			"solverStatus", out.Status,
			"pod", klog.KObj(pod),
			"node", out.NominatedNode,
			"planID", ap.ID,
			"moved", len(plan.Moves),
			"evicted", len(plan.Evicts),
			"preEnqueueDuration", time.Since(startTime),
			"solverDuration", solverDuration,
		)
		return framework.NewStatus(framework.Success, "PreEnqueue: nominated after plan execution")
	}

	if pl.Active.Load() {
		ap := pl.getActivePlan()

		// While a plan is executing, gate everything not explicitly allowed by the plan.
		if ap != nil && !ap.PlanDoc.Completed {
			if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
				return framework.NewStatus(framework.Success)
			}
			full := pod.Namespace + "/" + pod.Name
			if _, ok := ap.PlanDoc.PlacementsByName[full]; ok {
				return framework.NewStatus(framework.Success)
			}
			if wk, ok := topWorkload(pod); ok {
				if _, inPlan := ap.PlanDoc.WkDesiredPerNode[wk.String()]; inPlan {
					return framework.NewStatus(framework.Success)
				}
			}
			// New pods while executing → catch here regardless of BatchMode
			klog.InfoS("PreEnqueue: active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return framework.NewStatus(framework.Pending, "PreEnqueue: active plan; blocking")
		}
	} else if optimizeInBatches() && optimizeAtPreEnqueue() {
		klog.V(2).InfoS("PreEnqueue: batched pod", "pod", klog.KObj(pod))
		pl.Batched.AddPod(pod)
		return framework.NewStatus(framework.Pending, "PreEnqueue: batched pod")
	}

	// Allow pods when in mode optimizeinBatchesAtPostFilter or optimizeForEveryInPostFilter, as they will be catched in postfilter
	klog.V(V2).InfoS("PreEnqueue: no active plan; proceeding", "pod", klog.KObj(pod))
	return framework.NewStatus(framework.Success)
}
