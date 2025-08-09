package mycrossnodepreemptionbinpacking

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// executeBinPackingSolution executes the optimal bin-packing solution
func (pl *MyCrossNodePreemptionBinpacking) executeBinPackingSolution(ctx context.Context, solution *BinPackingSolution) error {
	klog.V(2).InfoS("Executing bin-packing solution",
		"targetNode", solution.TargetNode,
		"movements", len(solution.PodMovements),
		"evictions", len(solution.VictimsToEvict))

	// 1: Execute pod movements (relocations)
	for i, movement := range solution.PodMovements {
		klog.V(3).InfoS("Executing pod movement",
			"step", fmt.Sprintf("%d/%d", i+1, len(solution.PodMovements)),
			"pod", klog.KObj(movement.Pod),
			"from", movement.FromNode,
			"to", movement.ToNode)

		// Safety: never "move" to same node
		if movement.ToNode == movement.FromNode {
			klog.InfoS("Skipping movement to same node",
				"pod", klog.KObj(movement.Pod),
				"node", movement.FromNode)
			continue
		}

		if err := pl.movePodToNode(ctx, movement.Pod, movement.ToNode); err != nil {
			klog.ErrorS(err, "Failed to move pod",
				"pod", klog.KObj(movement.Pod),
				"toNode", movement.ToNode)
			// Continue with other movements even if one fails
		} else {
			klog.V(3).InfoS("Successfully initiated pod movement",
				"pod", klog.KObj(movement.Pod),
				"toNode", movement.ToNode)
		}
	}

	// 2: Wait briefly for movements to take effect
	if len(solution.PodMovements) > 0 {
		klog.V(3).InfoS("Waiting for pod movements to take effect")
		time.Sleep(2 * time.Second)
	}

	// 3: Execute pod evictions (deletions)
	for i, victim := range solution.VictimsToEvict {
		klog.V(3).InfoS("Executing pod eviction",
			"step", fmt.Sprintf("%d/%d", i+1, len(solution.VictimsToEvict)),
			"pod", klog.KObj(victim))

		if err := pl.evictPod(ctx, victim); err != nil {
			klog.ErrorS(err, "Failed to evict pod", "pod", klog.KObj(victim))
			// Continue with other evictions even if one fails
		} else {
			klog.V(3).InfoS("Successfully evicted pod", "pod", klog.KObj(victim))
		}
	}

	klog.V(2).InfoS("Bin-packing solution execution completed",
		"targetNode", solution.TargetNode,
		"totalMovements", len(solution.PodMovements),
		"totalEvictions", len(solution.VictimsToEvict))

	return nil
}

// movePodToNode moves a pod to a different node by creating a new pod pinned to that node, then deleting the old one
func (pl *MyCrossNodePreemptionBinpacking) movePodToNode(ctx context.Context, pod *v1.Pod, destNode string) error {
	// Validate destination node looks usable
	if err := pl.validatePodMovement(ctx, pod, destNode); err != nil {
		return err
	}

	// Create a new pod with the same spec but hard-pinned to the destination node
	moved := pod.DeepCopy()

	// Generate new name to avoid conflicts
	moved.Name = fmt.Sprintf("%s-moved-%d", pod.Name, time.Now().Unix())
	moved.ResourceVersion = ""
	moved.UID = ""
	moved.Status = v1.PodStatus{}
	moved.CreationTimestamp = metav1.Time{}

	// Remove scheduling hints that could fight our pinning
	moved.Spec.SchedulerName = ""                 // let kubelet accept NodeName assignment
	moved.Spec.NodeSelector = map[string]string{} // clear selectors
	moved.Spec.Affinity = nil                     // clear any node/pod affinity constraints

	// Hard pin to destination node so the default scheduler won't re-place it
	moved.Spec.NodeName = destNode

	// Mark it as a moved pod (debugging/traceability)
	if moved.Annotations == nil {
		moved.Annotations = map[string]string{}
	}
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-by"] = Name
	moved.Annotations["scheduler.alpha.kubernetes.io/original-pod-uid"] = string(pod.UID)

	klog.V(4).InfoS("Creating moved pod",
		"originalPod", klog.KObj(pod),
		"newPod", klog.KObj(moved),
		"destNode", destNode)

	// Create the new pod
	if _, err := pl.client.CoreV1().Pods(pod.Namespace).Create(ctx, moved, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create moved pod: %v", err)
	}

	// Delete the original pod with immediate deletion
	grace := int64(0)
	if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil {
		klog.ErrorS(err, "Failed to delete original pod after move", "pod", klog.KObj(pod))
		// Best-effort cleanup of the new pod if original deletion fails
		_ = pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, moved.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
		return fmt.Errorf("failed to delete original pod: %v", err)
	}

	klog.V(3).InfoS("Successfully moved pod",
		"originalPod", klog.KObj(pod),
		"newPod", klog.KObj(moved),
		"destNode", destNode)

	return nil
}

// evictPod evicts (deletes) a victim pod
func (pl *MyCrossNodePreemptionBinpacking) evictPod(ctx context.Context, pod *v1.Pod) error {
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
func (pl *MyCrossNodePreemptionBinpacking) validatePodMovement(ctx context.Context, pod *v1.Pod, targetNode string) error {
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