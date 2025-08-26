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

// PreFilter is called at the beginning of scheduling cycle.
// It is used, here, to filter the node(s) that the pod can be (tried) scheduled on.
// If a pod part of a plan was scheduled on a wrong node due to workload quotas,
// it is determined in Reserve plugin and will be retried again.
func (pl *MyCrossNodePreemption) PreFilter(ctx context.Context, st *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc.Completed {
		return nil, framework.NewStatus(framework.Success)
	}

	// Pin lead pod only if TargetNode is set (every-preemptor mode).
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		return &framework.PreFilterResult{NodeNames: sets.New(ap.PlanDoc.TargetNode)}, framework.NewStatus(framework.Success)
	}

	// Workload pods: allow nodes with remaining > 0
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := ap.PlanDoc.WkDesiredPerNode[key]; !inPlan {
			klog.V(2).InfoS("PreFilter: workload pod not in active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: workload pod not in active plan; blocking")
		}

		allowed := sets.New[string]()
		ap := pl.getActivePlan()
		if ap == nil { /* block or pass */
		}
		if byNode, ok := ap.Remaining[key]; ok {
			for node, ctr := range byNode {
				if ctr.Load() > 0 {
					allowed.Insert(node)
				}
			}
		}
		if allowed.Len() == 0 {
			klog.V(2).InfoS("PreFilter: workload-quotas exhausted; wait until plan is completed; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: workload-quotas exhausted; wait until plan is completed; blocking")
		}
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)
	}

	// Standalone pods: allow nodes explicitly placed by name and namespace
	full := pod.Namespace + "/" + pod.Name
	if tgt, ok := ap.PlanDoc.PlacementsByName[full]; ok {
		return &framework.PreFilterResult{NodeNames: sets.New(tgt)}, framework.NewStatus(framework.Success)
	}

	klog.V(2).InfoS("PreFilter: pod not in active plan; blocking", "pod", klog.KObj(pod))
	pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
	return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "PreFilter: pod not in active plan; blocking")
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
