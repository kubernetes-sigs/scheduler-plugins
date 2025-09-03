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
	// Always allow kube-system pods
	if pod.Namespace == "kube-system" {
		return nil, framework.NewStatus(framework.Success)
	}
	if !pl.IsActivePlan() {
		return nil, framework.NewStatus(framework.Success)
	}

	nodes, msg, ok := pl.preFilterAllowedNodes(pod)
	switch {
	case ok && nodes == nil:
		// allowed, no pin
		klog.V(V2).InfoS("PreFilter: allow", "pod", klog.KObj(pod), "reason", msg)
		return nil, framework.NewStatus(framework.Success)
	case ok && nodes.Len() > 0:
		klog.V(V2).InfoS("PreFilter: pin", "pod", klog.KObj(pod), "nodes", nodes.UnsortedList(), "reason", msg)
		return &framework.PreFilterResult{NodeNames: nodes}, framework.NewStatus(framework.Success)
	default: // not ok
		klog.V(V2).InfoS("PreFilter: block", "pod", klog.KObj(pod), "reason", msg)
		pl.Blocked.AddPod(pod)
		return nil, framework.NewStatus(framework.Unschedulable, "PreFilter: "+msg)
	}
}

func (pl *MyCrossNodePreemption) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// preFilterAllowedNodes returns:
// - node set to pin (non-nil) and Success, or
// - nil and an appropriate framework.Status reason to block/allow.
func (pl *MyCrossNodePreemption) preFilterAllowedNodes(pod *v1.Pod) (sets.Set[string], string, bool) {
	if !pl.IsActivePlan() {
		return nil, "no active plan", true // allow
	}
	ap := pl.getActivePlan()
	// Lead pod pinning (single-preemptor mode)
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		return sets.New(ap.PlanDoc.TargetNode), "lead; pin to target", true
	}

	// Workload quota routing
	if wk, ok := topWorkload(pod); ok {
		key := wk.String()
		if _, inPlan := ap.PlanDoc.WkDesiredPerNode[key]; !inPlan {
			return nil, "workload not in active plan; block", false
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
			return nil, "workload quotas exhausted; block", false
		}
		return allowed, "workload nodes allowed", true
	}

	// Standalone by name
	full := pod.Namespace + "/" + pod.Name
	if tgt, ok := ap.PlanDoc.PlacementsByName[full]; ok {
		return sets.New(tgt), "standalone; pin to planned node", true
	}

	return nil, "pod not in active plan; block", false
}
