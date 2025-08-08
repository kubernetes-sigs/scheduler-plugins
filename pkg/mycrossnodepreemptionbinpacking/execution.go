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

	// Phase 1: Execute pod movements (relocations)
	for i, movement := range solution.PodMovements {
		klog.V(3).InfoS("Executing pod movement",
			"step", fmt.Sprintf("%d/%d", i+1, len(solution.PodMovements)),
			"pod", klog.KObj(movement.Pod),
			"from", movement.FromNode,
			"to", movement.ToNode)

		err := pl.movePodToNode(ctx, movement.Pod, movement.ToNode)
		if err != nil {
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

	// Phase 2: Wait briefly for movements to take effect
	if len(solution.PodMovements) > 0 {
		klog.V(3).InfoS("Waiting for pod movements to take effect")
		time.Sleep(2 * time.Second)
	}

	// Phase 3: Execute pod evictions (deletions)
	for i, victim := range solution.VictimsToEvict {
		klog.V(3).InfoS("Executing pod eviction",
			"step", fmt.Sprintf("%d/%d", i+1, len(solution.VictimsToEvict)),
			"pod", klog.KObj(victim))

		err := pl.evictPod(ctx, victim)
		if err != nil {
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

// movePodToNode moves a pod to a different node by creating a new pod and deleting the old one
func (pl *MyCrossNodePreemptionBinpacking) movePodToNode(ctx context.Context, pod *v1.Pod, targetNode string) error {
	// Create a new pod with the same spec but targeted to the new node
	newPod := pod.DeepCopy()
	
	// Generate new name to avoid conflicts
	newPod.Name = fmt.Sprintf("%s-moved-%d", pod.Name, time.Now().Unix())
	newPod.ResourceVersion = ""
	newPod.UID = ""
	newPod.Status = v1.PodStatus{}
	newPod.CreationTimestamp = metav1.Time{}
	
	// Set node selector to target the destination node
	if newPod.Spec.NodeSelector == nil {
		newPod.Spec.NodeSelector = make(map[string]string)
	}
	newPod.Spec.NodeSelector["kubernetes.io/hostname"] = targetNode
	
	// Add annotation to indicate this is a moved pod
	if newPod.Annotations == nil {
		newPod.Annotations = make(map[string]string)
	}
	newPod.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
	newPod.Annotations["scheduler.alpha.kubernetes.io/moved-by"] = Name
	newPod.Annotations["scheduler.alpha.kubernetes.io/original-pod"] = string(pod.UID)

	klog.V(4).InfoS("Creating moved pod",
		"originalPod", klog.KObj(pod),
		"newPod", klog.KObj(newPod),
		"targetNode", targetNode)

	// Create the new pod
	_, err := pl.client.CoreV1().Pods(pod.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create moved pod: %v", err)
	}

	// Delete the original pod with immediate deletion
	err = pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &[]int64{0}[0], // Immediate deletion
	})
	if err != nil {
		klog.ErrorS(err, "Failed to delete original pod after move", 
			"pod", klog.KObj(pod))
		// Try to cleanup the new pod
		pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, newPod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &[]int64{0}[0],
		})
		return fmt.Errorf("failed to delete original pod: %v", err)
	}

	klog.V(3).InfoS("Successfully moved pod",
		"originalPod", klog.KObj(pod),
		"newPod", klog.KObj(newPod),
		"targetNode", targetNode)

	return nil
}

// evictPod evicts (deletes) a victim pod
func (pl *MyCrossNodePreemptionBinpacking) evictPod(ctx context.Context, pod *v1.Pod) error {
	klog.V(4).InfoS("Evicting victim pod", "pod", klog.KObj(pod))

	// Delete the pod with immediate deletion
	err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &[]int64{0}[0], // Immediate deletion
	})
	if err != nil {
		return fmt.Errorf("failed to delete victim pod: %v", err)
	}

	klog.V(3).InfoS("Successfully evicted pod", "pod", klog.KObj(pod))
	return nil
}

// validatePodMovement validates that a pod movement is safe and feasible
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

// calculateUtilizationImprovement calculates how much the bin-packing solution improves overall utilization
func (pl *MyCrossNodePreemptionBinpacking) calculateUtilizationImprovement(
	solution *BinPackingSolution,
	nodeStates map[string]*NodeResourceState,
) float64 {
	// Calculate utilization before and after the solution
	var totalBeforeUtilization, totalAfterUtilization float64
	nodeCount := float64(len(nodeStates))

	for nodeName, state := range nodeStates {
		// Before utilization
		totalBeforeUtilization += state.UtilizationCPU

		// After utilization (simulate the changes)
		afterUtilization := state.UtilizationCPU
		
		if nodeName == solution.TargetNode {
			// This node will have the new pod
			afterUtilization = float64(state.AllocatableCPU - state.AvailableCPU + getPodCPURequest(solution.VictimsToEvict[0])) / float64(state.AllocatableCPU)
		}

		// Account for moved pods
		for _, movement := range solution.PodMovements {
			if movement.FromNode == nodeName {
				// Pod moved away, utilization decreases
				afterUtilization -= float64(movement.CPURequest) / float64(state.AllocatableCPU)
			} else if movement.ToNode == nodeName {
				// Pod moved here, utilization increases
				afterUtilization += float64(movement.CPURequest) / float64(state.AllocatableCPU)
			}
		}

		totalAfterUtilization += afterUtilization
	}

	avgBeforeUtilization := totalBeforeUtilization / nodeCount
	avgAfterUtilization := totalAfterUtilization / nodeCount

	// Return the improvement (positive means better balanced utilization)
	return avgAfterUtilization - avgBeforeUtilization
}
