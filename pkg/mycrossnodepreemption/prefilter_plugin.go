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
	if pod.Namespace == "kube-system" {
		return nil, framework.NewStatus(framework.Success)
	}

	if !pl.Active.Load() {
		// Let the default scheduler proceed when there is no active plan.
		return nil, framework.NewStatus(framework.Success)
	}

	// Get active plan
	ap := pl.getActivePlan()
	if ap == nil {
		klog.V(V2).InfoS("PreFilter: no active plan; allowing", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.Success)
	}

	// Pin lead pod only if TargetNode is set (every-preemptor mode).
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		klog.V(V2).InfoS("PreFilter: lead pod; pinning to target node", "pod", klog.KObj(pod), "node", ap.PlanDoc.TargetNode)
		return &framework.PreFilterResult{NodeNames: sets.New(ap.PlanDoc.TargetNode)}, framework.NewStatus(framework.Success)
	}

	// Workload pods: allow nodes with remaining > 0
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := ap.PlanDoc.WkDesiredPerNode[key]; !inPlan {
			klog.InfoS("PreFilter: workload pod not in active plan; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: workload pod not in active plan; blocking")
		}

		allowed := sets.New[string]()
		if byNode, ok := ap.Remaining[key]; ok {
			for node, ctr := range byNode {
				if ctr.Load() > 0 {
					allowed.Insert(node)
				}
			}
		}
		if allowed.Len() == 0 {
			klog.V(V2).InfoS("PreFilter: workload-quotas exhausted; wait until plan is completed; blocking", "pod", klog.KObj(pod))
			pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
			return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: workload-quotas exhausted; wait until plan is completed; blocking")
		}
		return &framework.PreFilterResult{NodeNames: allowed}, framework.NewStatus(framework.Success)
	}

	// Standalone pods: allow nodes explicitly placed by name and namespace
	full := pod.Namespace + "/" + pod.Name
	if tgt, ok := ap.PlanDoc.PlacementsByName[full]; ok {
		klog.InfoS("PreFilter: standalone pod; pinning to planned node", "pod", klog.KObj(pod), "node", tgt)
		return &framework.PreFilterResult{NodeNames: sets.New(tgt)}, framework.NewStatus(framework.Success)
	}

	klog.InfoS("PreFilter: pod not in active plan; blocking", "pod", klog.KObj(pod))
	pl.Blocked.AddRef(pod.UID, pod.Namespace, pod.Name)
	return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: pod not in active plan; blocking")

}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
