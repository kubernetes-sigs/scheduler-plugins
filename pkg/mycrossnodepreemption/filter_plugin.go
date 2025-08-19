// filter_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// During an active plan (“stop-the-world”):
// - Pending (preemptor) pod: only allowed on TargetNode (scheduler binds it).
// - RS pods: only allowed up to their per-node quota (RSDesiredPerNode).
// - Standalone pods named in PlacementsByName: only on their planned node.
// - Everyone else: blocked until plan completes.
func (pl *MyCrossNodePreemption) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
) *framework.Status {

	sp, _, err := pl.loadActivePlan(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to load active plan")
		return framework.NewStatus(framework.Error, err.Error())
	}
	if sp == nil || !sp.StopTheWorld || sp.Completed {
		return framework.NewStatus(framework.Success, "")
	}

	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Unschedulable, "node info missing")
	}
	nodeName := nodeInfo.Node().Name

	// 0) Pending preemptor: gate to the nominated node only.
	if string(pod.UID) == sp.PendingUID {
		if nodeName == sp.TargetNode {
			klog.V(2).InfoS("Filter", "pod", klog.KObj(pod), "target node", nodeName)
			return framework.NewStatus(framework.Success, "")
		}
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: pending pod only allowed on target node")
	}

	// 1) RS-owned pods: enforce per-node slots
	if rsName, ok := owningReplicaSet(pod); ok {
		key := rsKey(pod.Namespace, rsName)
		targets, ok := sp.RSDesiredPerNode[key]
		if !ok {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS not in active plan")
		}
		desired := targets[nodeName]
		if desired == 0 {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS has no slots on this node")
		}
		current := 0
		for _, pi := range nodeInfo.Pods {
			if pi.Pod.Namespace == pod.Namespace {
				if r, ok := owningReplicaSet(pi.Pod); ok && r == rsName {
					current++
				}
			}
		}
		if current >= desired {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS node quota reached")
		}
		klog.V(2).InfoS("Filter", "pod", klog.KObj(pod), "target node", nodeName)
		return framework.NewStatus(framework.Success, "")
	}

	// 2) Standalone pods by name
	if tgt, ok := sp.PlacementsByName[pod.Name]; ok {
		if tgt == nodeName {
			klog.V(2).InfoS("Filter", "pod", klog.KObj(pod), "target node", nodeName)
			return framework.NewStatus(framework.Success, "")
		}
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: only planned node allowed")
	}

	// 3) Everyone else waits
	return framework.NewStatus(framework.Unschedulable, "stop-the-world: pod not in active plan")
}
