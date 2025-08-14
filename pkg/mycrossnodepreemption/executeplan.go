package mycrossnodepreemption

import (
	"context"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type nodeCap struct {
    allocCPU int64
    allocMem int64
    freeCPU  int64
    freeMem  int64
}

// executePlan executes the optimal pod-assignment plan
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan) error {
    klog.V(2).InfoS("Executing pod-assignment plan",
        "targetNode", plan.TargetNode,
        "movements", len(plan.PodMovements),
        "evictions", len(plan.VictimsToEvict))
    
    // Counters for success/failure
	var (
		evictOK, evictFail int
		moveOK, moveFail   int
	)

    // 0) Build live per-node capacity ledger
    niList, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
    if err != nil {
        return fmt.Errorf("list nodes: %w", err)
    }
    caps := map[string]*nodeCap{}
    for _, ni := range niList {
        freeCPU := ni.Allocatable.MilliCPU - ni.Requested.MilliCPU
        freeMem := ni.Allocatable.Memory - ni.Requested.Memory
        if freeCPU < 0 { freeCPU = 0 }
        if freeMem < 0 { freeMem = 0 }
        caps[ni.Node().Name] = &nodeCap{
            allocCPU: ni.Allocatable.MilliCPU,
            allocMem: ni.Allocatable.Memory,
            freeCPU:  freeCPU,
            freeMem:  freeMem,
        }
    }

	// 1) Evict first
	for i, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Evicting victim",
			"step", fmt.Sprintf("%d/%d", i+1, len(plan.VictimsToEvict)),
			"pod", podRef(v),
			"node", v.Spec.NodeName)
		if err := pl.evictPod(ctx, v); err != nil {
			klog.ErrorS(err, "Eviction failed", "pod", podRef(v))
			evictFail++
			// keep going
		} else {
			n := v.Spec.NodeName
			if c := caps[n]; c != nil {
				c.freeCPU += getPodCPURequest(v)
				c.freeMem += getPodMemoryRequest(v)
			}
			evictOK++
			klog.V(2).InfoS("Eviction succeeded", "pod", podRef(v))
		}
	}

	// 2) Order & execute moves greedily
	remaining := append([]PodMovement(nil), plan.PodMovements...)
	madeProgress := true

	for len(remaining) > 0 && madeProgress {
		madeProgress = false
		next := remaining[:0]

		for _, mv := range remaining {
			needCPU := mv.CPURequest
			needMem := mv.MemoryRequest
			dst := caps[mv.ToNode]
			if dst != nil && dst.freeCPU >= needCPU && dst.freeMem >= needMem {
				klog.V(2).InfoS("Attempting move",
					"pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
				if err := pl.movePodToNodeDeleteFirst(ctx, mv.Pod, mv.ToNode); err != nil {
					klog.ErrorS(err, "Move failed", "pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
					moveFail++
					next = append(next, mv) // try later or deadlock-breaker
					continue
				}
				// Update ledgers
				if sc := caps[ mv.FromNode ]; sc != nil {
					sc.freeCPU += needCPU
					sc.freeMem += needMem
				}
				dst.freeCPU -= needCPU
				dst.freeMem -= needMem
				moveOK++
				madeProgress = true
				klog.V(2).InfoS("Move succeeded", "pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
			} else {
				next = append(next, mv)
			}
		}
		remaining = next
	}

	if len(remaining) > 0 {
		// Deadlock breaker: one delete-first to create space, then recurse
		klog.InfoS("Move deadlock detected; breaking with delete-first", "remainingMoves", len(remaining))
		mv := remaining[0]
		if err := pl.movePodToNodeDeleteFirst(ctx, mv.Pod, mv.ToNode); err != nil {
			moveFail++
			klog.ErrorS(err, "Deadlock break move failed", "pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
			return fmt.Errorf("failed to break deadlock by moving %s: %w", podRef(mv.Pod), err)
		}
		moveOK++
		klog.V(2).InfoS("Deadlock break move succeeded", "pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)

		// Re-run for remaining
		subPlan := &PodAssignmentPlan{TargetNode: plan.TargetNode, PodMovements: remaining[1:]}
		if err := pl.executePlan(ctx, subPlan); err != nil {
			// Bubble up but log a summary first
			klog.ErrorS(err, "Plan execution failed after deadlock break",
				"movesOK", moveOK, "movesFailed", moveFail, "evictionsOK", evictOK, "evictionsFailed", evictFail)
			return err
		}
	}

	klog.InfoS("Bin-packing execution summary",
		"targetNode", plan.TargetNode,
		"movesOK", moveOK, "movesFailed", moveFail,
		"evictionsOK", evictOK, "evictionsFailed", evictFail)

	if moveFail == 0 && evictFail == 0 {
		klog.InfoS("Plan executed successfully")
	} else {
		klog.ErrorS(nil, "Plan executed with failures",
			"movesFailed", moveFail, "evictionsFailed", evictFail)
	}

	return nil
}

// delete-first: free capacity before recreating
func (pl *MyCrossNodePreemption) movePodToNodeDeleteFirst(ctx context.Context, pod *v1.Pod, destNode string) error {
    if destNode == pod.Spec.NodeName {
        klog.V(4).InfoS("Skip no-op move", "pod", klog.KObj(pod), "node", destNode)
        return nil
    }
    if err := pl.validatePodMovement(ctx, pod, destNode); err != nil {
        return err
    }

    // 1) Delete original (grace 0)
    grace := int64(0)
    if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
        GracePeriodSeconds: &grace,
    }); err != nil {
        return fmt.Errorf("delete original before move: %w", err)
    }

    // 2) Recreate pinned at destination
    moved := pod.DeepCopy()
    moved.ResourceVersion = ""
    moved.Status = v1.PodStatus{}
    moved.Spec.NodeSelector = map[string]string{} // TODO_HC: clear selectors, however, see if can keep them
    moved.Spec.Affinity = nil // TODO_HC: clear affinity, however, see if can keep them
    moved.Spec.NodeName = destNode

    // Log movement details in pod annotations
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-timestamp"] = time.Now().Format(time.RFC3339)
	if _, ok := moved.Annotations["scheduler.alpha.kubernetes.io/original-node"]; !ok {
		moved.Annotations["scheduler.alpha.kubernetes.io/original-node"] = pod.Spec.NodeName
	}
    // Log number of movements
    moveCountStr := moved.Annotations["scheduler.alpha.kubernetes.io/move-count"]
    moveCount, _ := strconv.Atoi(moveCountStr)
    moveCount++
    moved.Annotations["scheduler.alpha.kubernetes.io/move-count"] = fmt.Sprintf("%d", moveCount)

    if _, err := pl.client.CoreV1().Pods(pod.Namespace).Create(ctx, moved, metav1.CreateOptions{}); err != nil {
        return fmt.Errorf("create moved pod: %w", err)
    }
    return nil
}

// evictPod evicts (deletes) a victim pod
func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	klog.V(4).InfoS("Evicting victim pod", "pod", klog.KObj(pod))

	grace := int64(0)
	if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace, // Immediate deletion
	}); err != nil {
		return fmt.Errorf("failed to delete victim pod: %v", err)
	}

	klog.V(3).InfoS("Successfully evicted pod", "pod", klog.KObj(pod))
	return nil
}

// validatePodMovement validates that a pod movement is safe and feasible.
// By safe and feasible, we mean that the target node must be ready, schedulable, and have enough resources.
func (pl *MyCrossNodePreemption) validatePodMovement(ctx context.Context, pod *v1.Pod, targetNode string) error {
	// Check if target node exists and is ready
	node, err := pl.client.CoreV1().Nodes().Get(ctx, targetNode, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("target node %s not found: %v", targetNode, err)
	}

	// Check if node is ready
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady && condition.Status != v1.ConditionTrue {
			return fmt.Errorf("target node %s is not ready", targetNode)
		}
	}

	// Check if node is schedulable
	if node.Spec.Unschedulable {
		return fmt.Errorf("target node %s is unschedulable", targetNode)
	}

	return nil
}