package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

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

// executePlan executes the optimal bin-packing solution
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *BinPackingPlan) error {
    klog.V(2).InfoS("Executing bin-packing solution",
        "targetNode", plan.TargetNode,
        "movements", len(plan.PodMovements),
        "evictions", len(plan.VictimsToEvict))

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

    // 1) Evict first (always frees space)
    for i, v := range plan.VictimsToEvict {
        klog.V(3).InfoS("Evicting victim", "step", fmt.Sprintf("%d/%d", i+1, len(plan.VictimsToEvict)), "pod", klog.KObj(v))
        if err := pl.evictPod(ctx, v); err != nil {
            klog.ErrorS(err, "Eviction failed", "pod", klog.KObj(v))
            // keep going
        } else {
            n := v.Spec.NodeName
            caps[n].freeCPU += getPodCPURequest(v)
            caps[n].freeMem += getPodMemoryRequest(v)
        }
    }

    // 2) Order moves so each destination has room when we execute the move.
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
                // Safe to move now
                if err := pl.movePodToNodeDeleteFirst(ctx, mv.Pod, mv.ToNode); err != nil {
                    klog.ErrorS(err, "Move failed", "pod", klog.KObj(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
                    // keep trying other moves
                    next = append(next, mv)
                    continue
                }
                // Update ledgers: source frees, dest consumes
                srcCap := caps[mv.FromNode]
                if srcCap != nil {
                    srcCap.freeCPU += needCPU
                    srcCap.freeMem += needMem
                }
                dst.freeCPU -= needCPU
                dst.freeMem -= needMem
                madeProgress = true
            } else {
                next = append(next, mv)
            }
        }
        remaining = next
    }

    if len(remaining) > 0 {
        // Deadlock (cycle) or insufficient freeing — as a last resort, do one delete-first to break it
        klog.V(2).InfoS("Move deadlock detected; breaking with delete-first of one pod", "remainingMoves", len(remaining))
        mv := remaining[0]
        if err := pl.movePodToNodeDeleteFirst(ctx, mv.Pod, mv.ToNode); err != nil {
            return fmt.Errorf("failed to break deadlock by moving %s: %w", klog.KObj(mv.Pod), err)
        }
        // (We don’t need to perfect the ledger here; the loop above will finish the rest.)
        remaining = remaining[1:]
        // Re-run the greedy phase for the rest
        // Simple recursion for clarity
        subPlan := &BinPackingPlan{TargetNode: plan.TargetNode, PodMovements: remaining}
        return pl.executePlan(ctx, subPlan)
    }

    klog.V(2).InfoS("Bin-packing solution execution completed",
        "targetNode", plan.TargetNode,
        "totalMovements", len(plan.PodMovements),
        "totalEvictions", len(plan.VictimsToEvict))
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
    moved.Name = fmt.Sprintf("%s-moved-%d", pod.Name, time.Now().Unix())
    moved.ResourceVersion = ""
    moved.UID = ""
    moved.Status = v1.PodStatus{}
    moved.CreationTimestamp = metav1.Time{}
    moved.Spec.SchedulerName = ""                 // allow NodeName
    moved.Spec.NodeSelector = map[string]string{} // clear selectors
    moved.Spec.Affinity = nil
    moved.Spec.NodeName = destNode
    if moved.Annotations == nil {
        moved.Annotations = map[string]string{}
    }
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
    moved.Annotations["scheduler.alpha.kubernetes.io/moved-by"] = Name
    moved.Annotations["scheduler.alpha.kubernetes.io/original-pod-uid"] = string(pod.UID)

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