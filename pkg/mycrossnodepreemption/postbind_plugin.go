// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
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
	if err != nil || sp == nil || !sp.StopTheWorld || sp.Completed {
		return
	}

	fail := func(reason string) {
		klog.ErrorS(nil, "PostBind plan violation; deactivating active plan",
			"pod", podRef(pod), "node", nodeName, "reason", reason)
		pl.markPlanCompleted(ctx, cmName)
	}

	// 1) Pending preemptor must bind to TargetNode
	if string(pod.UID) == sp.PendingUID {
		if nodeName != sp.TargetNode {
			fail("pending pod bound to non-target node")
			return // violation -> stop
		}
		// ✅ Correct bind: do NOT return here.
		// Fall through to the completion check below.
	}

	// 2) Standalone planned-by-name must bind to its planned node
	if tgt, ok := sp.PlacementsByName[pod.Name]; ok {
		if nodeName != tgt {
			fail("standalone pod bound to wrong node")
			return
		}
		// fall through to completion check
	}

	// 3) RS pods: ensure this bind does not exceed per-node target
	if rsName, ok := owningReplicaSet(pod); ok {
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

	// ✅ Always run a *live* completion check after any successful bind.
	if ok, err := pl.planLooksCompleteLive(ctx, sp); err != nil {
		klog.ErrorS(err, "PostBind: completion check failed")
	} else if ok {
		klog.InfoS("PostBind: plan completed; lifting stop-the-world")
		pl.markPlanCompleted(ctx, cmName)
	} else {
		klog.V(2).InfoS("PostBind: plan still active")
	}
}

// returns (completed, err)
func (pl *MyCrossNodePreemption) updateProgressOnBind(ctx context.Context, pod *v1.Pod, nodeName string) (bool, error) {
	// Load latest CM and doc with conflict retry
	var completed bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Find active plan CM (same as loadActivePlan, but give me the CM object)
		list, err := pl.client.CoreV1().ConfigMaps(exportNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", exportCMLabelActiveKey, exportCMLabelActiveTrue),
		})
		if err != nil {
			return err
		}
		if len(list.Items) == 0 {
			// no active plan; nothing to do
			completed = false
			return nil
		}
		// pick newest
		cm := list.Items[0]
		raw := cm.Data["plan.json"]
		if raw == "" {
			return fmt.Errorf("active plan missing plan.json")
		}
		var sp StoredPlan
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			return err
		}
		if !sp.StopTheWorld || sp.Completed || sp.Progress == nil {
			completed = false
			return nil
		}

		prog := sp.Progress

		// 1) Pending preemptor?
		if string(pod.UID) == sp.PendingUID && nodeName == sp.TargetNode {
			prog.PendingBound = true
		}

		// 2) Standalone?
		if tgt, ok := sp.PlacementsByName[pod.Name]; ok && tgt == nodeName {
			ns := pod.Namespace
			key := nsNameKey(ns, pod.Name)
			prog.StandaloneOK[key] = true
		}

		// 3) RS decrement?
		if rsName, ok := owningReplicaSet(pod); ok {
			key := rsKey(pod.Namespace, rsName)
			if perNode, ok := prog.RSRemaining[key]; ok {
				if left, ok := perNode[nodeName]; ok && left > 0 {
					perNode[nodeName] = left - 1
				}
			}
		}

		// Check completion
		done := prog.PendingBound
		if done {
			// all standalone satisfied?
			for _, ok := range prog.StandaloneOK {
				if !ok {
					done = false
					break
				}
			}
		}
		if done {
			// all RSRemaining zero?
		outer:
			for _, perNode := range prog.RSRemaining {
				for _, left := range perNode {
					if left > 0 {
						done = false
						break outer
					}
				}
			}
		}
		if done {
			completed = true
			// Mark completed by deleting the CM after we return
		} else {
			completed = false
			// Write updated progress back to CM
			// (leave Completed=false; StopTheWorld=true)
			// Re-marshal and update the existing CM
			b, err := json.MarshalIndent(&sp, "", "  ")
			if err != nil {
				return err
			}
			cm.Data["plan.json"] = string(b)
			if _, err := pl.client.CoreV1().ConfigMaps(exportNamespace).Update(ctx, &cm, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}
