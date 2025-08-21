// prefilter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreFilterPlugin = &MyCrossNodePreemption{}

// PreFilter filters nodes for the pod according to active plan.
// If no active plan, allow scheduling for all nodes.
func (pl *MyCrossNodePreemption) PreFilter(ctx context.Context, st *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	sp, planID := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return nil, framework.NewStatus(framework.Success)
	}

	// Check if the pending pod is the one we are waiting for
	if string(pod.UID) == sp.PendingUID {
		return &framework.PreFilterResult{NodeNames: sets.New(sp.TargetNode)}, framework.NewStatus(framework.Success)
	}

	// RS pods: allow nodes with remaining > 0
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := sp.WorkloadDesiredPerNode[key]; !inPlan {
			pl.blocked.add(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: RS not in active plan")
		}

		// Allow nodes not yet allocated to their quotas
		allowed := sets.New[string]()
		slots := pl.slotsPtr.Load()
		if slots != nil && slots.planID == planID {
			if byNode, ok := slots.remaining[key]; ok {
				for node, ctr := range byNode {
					if ctr.Load() > 0 {
						allowed.Insert(node)
					}
				}
			}
		}
		if allowed.Len() == 0 {
			pl.blocked.add(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: RS-quotas exhausted; wait until plan is completed")
		}
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)
	}

	// Standalone pods: allow nodes explicitly placed by name and namespace
	full := pod.Namespace + "/" + pod.Name
	if tgt, ok := sp.PlacementsByName[full]; ok {
		return &framework.PreFilterResult{NodeNames: sets.New(tgt)}, framework.NewStatus(framework.Success)
	}

	// Everyone else must wait until the plan is completed
	pl.blocked.add(pod.UID, pod.Namespace, pod.Name)
	return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: RS/pod not in active plan")
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
