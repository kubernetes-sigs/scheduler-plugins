// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreEnqueuePlugin = &MyCrossNodePreemption{}

func (pl *MyCrossNodePreemption) PreEnqueue(
	_ context.Context,
	pod *v1.Pod,
) *framework.Status {
	sp, _ := pl.getActivePlan()

	// Always allow kube-system to proceed.
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}

	// When a plan is active, stop-the-world except whitelisted pods.
	if sp != nil && !sp.Completed {
		if string(pod.UID) == sp.PendingUID {
			return framework.NewStatus(framework.Success)
		}
		full := pod.Namespace + "/" + pod.Name
		if _, ok := sp.PlacementsByName[full]; ok {
			return framework.NewStatus(framework.Success)
		}
		if wk, ok := topWorkload(pod); ok {
			key := wk.String()
			if _, inPlan := sp.WorkloadDesiredPerNode[key]; inPlan {
				return framework.NewStatus(framework.Success)
			}
			pl.blockedPods.add(pod.UID, pod.Namespace, pod.Name)
			return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: RS not in active plan")
		}
		pl.blockedPods.add(pod.UID, pod.Namespace, pod.Name)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: pod not in active plan")
	}

	// No active plan:
	if BatchModeEnabled {
		pl.addToBatch(pod)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: batched for cross-node optimization")
	}

	// Not batching: let it flow; if unschedulable, PostFilter may solve single-preemptor.
	return framework.NewStatus(framework.Success)
}
