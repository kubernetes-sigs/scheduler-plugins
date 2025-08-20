// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind runs after a pod is bound by the scheduler. We double-check
// that the binding is consistent with the active plan. If not, we
// deactivate the plan immediately and log an error.
//
// NOTE: PostBind has no return status; to “stop our plugin” we lift
// the stop-the-world by deleting the active plan ConfigMap so the
// Filter no longer holds the cluster.
func (pl *MyCrossNodePreemption) PostBind(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeName string,
) {
	sp, cmName, err := pl.loadActivePlan(ctx)
	if err != nil || sp == nil || sp.Completed {
		return
	}

	fail := func(reason string) {
		klog.ErrorS(nil, "PostBind: plan violation; deactivating active plan",
			"pod", podRef(pod), "node", nodeName, "reason", reason)
		pl.markPlanCompleted(ctx, cmName)
	}

	// 1) Pending preemptor must bind to TargetNode
	if string(pod.UID) == sp.PendingUID {
		if nodeName != sp.TargetNode {
			fail("pending pod bound to non-target node")
			return // violation -> stop
		}
		// 2) Standalone planned-by-name must bind to its planned node
	} else if tgt, ok := sp.PlacementsByName[pod.Name]; ok {
		if nodeName != tgt {
			fail("standalone pod bound to wrong node")
			return
		}
		// 3) RS pods: ensure this bind does not exceed per-node target
	} else if rsName, ok := owningReplicaSet(pod); ok {
		key := rsKey(pod.Namespace, rsName)
		if perNode, ok := sp.RSDesiredPerNode[key]; ok {
			want := perNode[nodeName]
			if want == 0 {
				fail("RS pod bound on node without a slot")
				return
			}
			ni, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
			if err != nil || ni == nil || ni.Node() == nil {
				fail("unable to validate RS per-node count after bind")
				return
			}
			cur := 0
			for _, pi := range ni.Pods {
				if pi.Pod.Namespace == pod.Namespace {
					if r, ok := owningReplicaSet(pi.Pod); ok && r == rsName {
						cur++
					}
				}
			}
			if cur > want+1 {
				fail("RS per-node target exceeded")
				return
			}
		}
	}

	// Always run a completion check after any successful bind.
	if ok, err := pl.isPlanCompleted(ctx, sp); err != nil {
		klog.ErrorS(err, "PostBind: completion check failed")
	} else if ok {
		klog.InfoS("PostBind: plan completed")
		pl.markPlanCompleted(ctx, cmName)
	} else {
		klog.V(2).InfoS("PostBind: plan still active")
	}
}
