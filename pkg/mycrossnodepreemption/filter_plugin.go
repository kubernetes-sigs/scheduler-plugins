package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Enforces the active plan during stop-the-world:
//   - RS pods: allow only where the RS still has a free slot on that node
//   - Standalone pods: allow only on their planned node (by name)
//   - All other pods: blocked until plan completes
func (pl *MyCrossNodePreemption) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
) *framework.Status {

	sp, cmName, err := pl.loadActivePlan(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to load active plan")
		return framework.NewStatus(framework.Error, err.Error())
	}
	if sp == nil || !sp.StopTheWorld || sp.Completed {
		return framework.NewStatus(framework.Success, "")
	}

	// Early exit if plan completed
	if pl.planLooksComplete(sp) {
		pl.markPlanCompleted(ctx, cmName)
		return framework.NewStatus(framework.Success, "")
	}

	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Unschedulable, "node info missing")
	}
	nodeName := nodeInfo.Node().Name

	// RS-owned pods
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
		return framework.NewStatus(framework.Success, "")
	}

	// Standalone pods by name
	if tgt, ok := sp.PlacementsByName[pod.Name]; ok {
		if tgt == nodeName {
			return framework.NewStatus(framework.Success, "")
		}
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: only planned node allowed")
	}

	// Everything else is held during stop-the-world
	return framework.NewStatus(framework.Unschedulable, "stop-the-world: pod not in active plan")
}
