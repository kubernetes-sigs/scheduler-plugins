// preenqueue_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Compile-time check.
var _ framework.PreEnqueuePlugin = &MyCrossNodePreemption{}

// PreEnqueue gates what goes to ActiveQ while a plan is active.
func (pl *MyCrossNodePreemption) PreEnqueue(
	_ context.Context,
	pod *v1.Pod,
) *framework.Status {
	sp, _ := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return framework.NewStatus(framework.Success)
	}

	// Always let kube-system proceed to avoid stalling cluster health.
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}

	// Allow the pending preemptor.
	if string(pod.UID) == sp.PendingUID {
		return framework.NewStatus(framework.Success)
	}

	// Allow standalone pods explicitly placed by name.
	full := pod.Namespace + "/" + pod.Name
	if _, ok := sp.PlacementsByName[full]; ok {
		return framework.NewStatus(framework.Success)
	}

	// Allow workload-owned pods only if their ReplicaSet is part of the plan.
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := sp.WorkloadDesiredPerNode[key]; inPlan {
			return framework.NewStatus(framework.Success)
		}
		pl.blocked.add(pod.UID, pod.Namespace, pod.Name)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: RS not in active plan")
	}

	// Everything else waits while the plan executes.
	pl.blocked.add(pod.UID, pod.Namespace, pod.Name)
	return framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreEnqueue: pod not in active plan")
}
