// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreEnqueuePlugin = &MyCrossNodePreemption{}

// PreEnqueue is called before a pod is enqueued (first point of contact).
// It is used, here, to determine if a pod can be enqueued or if it should be blocked/batched according to the active plan.
func (pl *MyCrossNodePreemption) PreEnqueue(_ context.Context, pod *v1.Pod) *framework.Status {
	_ = pl.pruneStaleSetEntries(pl.Blocked)
	ap := pl.getActivePlan()

	// Always allow kube-system to proceed.
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}

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
		klog.V(2).InfoS("PreEnqueue: active plan; blocking", "pod", klog.KObj(pod))
		pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: active plan; blocking")
	}

	// No active plan:
	if strategyBatchAtPreEnqueue() {
		klog.V(2).InfoS("PreEnqueue: batched pod", "pod", klog.KObj(pod))
		pl.Batched.AddPod(pod)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: batched pod")
	}

	// BatchPostFilter or BatchOff, let it flow; if it fails, PostFilter will catch (or every-preemptor will act).
	return framework.NewStatus(framework.Success)
}
