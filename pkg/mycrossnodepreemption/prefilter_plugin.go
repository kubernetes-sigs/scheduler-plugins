// prefilter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.PreFilterPlugin = &MyCrossNodePreemption{}

func (pl *MyCrossNodePreemption) PreFilter(ctx context.Context, st *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	sp, planID := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return nil, framework.NewStatus(framework.Success)
	}

	// Pin the lead pod only if TargetNode is set (every-preemptor mode).
	if sp.TargetNode != "" && string(pod.UID) == sp.PendingUID {
		return &framework.PreFilterResult{NodeNames: sets.New(sp.TargetNode)}, framework.NewStatus(framework.Success)
	}

	// Top-Workload pods: allow nodes with remaining > 0
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := sp.WorkloadDesiredPerNode[key]; !inPlan {
			klog.V(2).InfoS("PreFilter: top-workload pod not in active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: top-workload pod not in active plan; blocking")
		}

		allowed := sets.New[string]()
		slots := pl.SlotsPtr.Load()
		if slots != nil && slots.PlanID == planID {
			if byNode, ok := slots.Remaining[key]; ok {
				for node, ctr := range byNode {
					if ctr.Load() > 0 {
						allowed.Insert(node)
					}
				}
			}
		}
		if allowed.Len() == 0 {
			klog.V(2).InfoS("PreFilter: top-workload-quotas exhausted; wait until plan is completed; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: top-workload-quotas exhausted; wait until plan is completed; blocking")
		}
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)
	}

	// Standalone pods: allow nodes explicitly placed by name and namespace
	full := pod.Namespace + "/" + pod.Name
	if tgt, ok := sp.PlacementsByName[full]; ok {
		return &framework.PreFilterResult{NodeNames: sets.New(tgt)}, framework.NewStatus(framework.Success)
	}

	klog.V(2).InfoS("PreFilter: pod not in active plan; blocking", "pod", klog.KObj(pod))
	pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
	return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: pod not in active plan; blocking")
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
