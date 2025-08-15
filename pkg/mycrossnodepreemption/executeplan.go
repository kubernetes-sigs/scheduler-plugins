package mycrossnodepreemption

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type nodeCap struct {
	allocCPU int64
	allocMem int64
	freeCPU  int64
	freeMem  int64
}

var ErrNoRoom = fmt.Errorf("destination has no room")

// executePlan executes the optimal pod-assignment plan
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan) error {
	var evictOK, evictFail, moveOK, moveFail int

	// Evict planned victims first (free space)
	for i, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Evicting victim",
			"step", fmt.Sprintf("%d/%d", i+1, len(plan.VictimsToEvict)),
			"pod", podRef(v), "node", v.Spec.NodeName)
		if err := pl.evictPod(ctx, v); err != nil {
			klog.ErrorS(err, "Eviction failed", "pod", podRef(v))
			evictFail++
		} else {
			evictOK++
		}
	}

	// Perform moves exactly as the solver planned (trust the plan)
	var pending []PodMovement
	// group by dest
	// Two concurrent moves to the same node can both pass the pre‑check and then collectively overcommit. Do them sequentially per destination:
	byDest := map[string][]PodMovement{}
	for _, mv := range plan.PodMovements {
		byDest[mv.ToNode] = append(byDest[mv.ToNode], mv)
	}
	for dest, mvs := range byDest {
		for i, mv := range mvs {
			klog.V(2).InfoS("Moving pod", "dest", dest, "step", fmt.Sprintf("%d/%d", i+1, len(mvs)), "pod", podRef(mv.Pod))
			if err := pl.movePodFast(ctx, mv.Pod, dest); err != nil {
				if errors.Is(err, ErrNoRoom) {
					pending = append(pending, mv)
					continue
				}
				moveFail++
				return fmt.Errorf("move failed for %s: %w", podRef(mv.Pod), err)
			}
			moveOK++
		}
	}

	if len(pending) > 0 {
		// One retry pass
		made := 0
		for _, mv := range pending {
			if err := pl.movePodFast(ctx, mv.Pod, mv.ToNode); err != nil {
				// Still no room → stop early with an explicit reason
				return fmt.Errorf("cannot place %s on %s yet: %w", podRef(mv.Pod), mv.ToNode, err)
			}
			made++
		}
		moveOK += made
	}

	klog.InfoS("Plan execution summary",
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

func (pl *MyCrossNodePreemption) movePodFast(ctx context.Context, pod *v1.Pod, destNode string) error {
	if destNode == pod.Spec.NodeName {
		klog.V(4).InfoS("Skip no-op move", "pod", klog.KObj(pod), "node", destNode)
		return nil
	}
	// Basic safety: node exists, Ready, schedulable
	if err := pl.validatePodMovement(ctx, pod, destNode); err != nil {
		return err
	}

	// We check *before* deleting the source pod to avoid needless disruption.
	probe := pod.DeepCopy()
	probe.Spec.NodeName = destNode
	probe.Spec.NodeSelector = map[string]string{} // avoid conflicts
	probe.Spec.Affinity = nil
	if err := pl.verifyNodeHasRoomFor(ctx, destNode, probe); err != nil {
		return fmt.Errorf("dest %s has no room for %s: %w", destNode, podRef(pod), err)
	}

	// Delete original (grace 0)
	grace := int64(0)
	if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil {
		return fmt.Errorf("delete original before move: %w", err)
	}
	if err := pl.waitForPodGone(ctx, pod.Namespace, pod.Name, 30*time.Second); err != nil {
		return fmt.Errorf("wait for old pod to disappear: %w", err)
	}

	moved := pod.DeepCopy()
	moved.ResourceVersion = ""
	moved.UID = ""
	moved.Status = v1.PodStatus{}
	moved.Spec.SchedulerName = ""
	moved.Spec.NodeName = destNode
	moved.Spec.NodeSelector = map[string]string{}
	moved.Spec.Affinity = nil
	if moved.Annotations == nil {
		moved.Annotations = map[string]string{}
	}
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-timestamp"] = time.Now().Format(time.RFC3339)

	if _, err := pl.client.CoreV1().Pods(pod.Namespace).Create(ctx, moved, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create moved pod: %w", err)
	}
	return nil
}

// evictPod deletes a victim pod; if it's standalone (no controller), recreate a Pending copy
// so it can be scheduled later when capacity appears.
func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	klog.V(4).InfoS("Evicting victim pod", "pod", klog.KObj(pod))

	grace := int64(0)
	if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil {
		return fmt.Errorf("failed to delete victim pod: %v", err)
	}

	// If a controller owns this pod, it’ll recreate it automatically.
	// If it's a naked pod, recreate a Pending copy ourselves so it can be scheduled later.
	if !hasController(pod) {
		// Wait for resource name to be fully released to reuse the same name
		if err := pl.waitForPodGone(ctx, pod.Namespace, pod.Name, 30*time.Second); err != nil {
			return fmt.Errorf("wait for evicted pod to disappear: %w", err)
		}
		if err := pl.recreatePendingCopy(ctx, pod); err != nil {
			return fmt.Errorf("recreate pending copy: %w", err)
		}
		klog.V(3).InfoS("Recreated pending copy for standalone pod", "pod", klog.KObj(pod))
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

func (pl *MyCrossNodePreemption) verifyNodeHasRoomFor(ctx context.Context, nodeName string, pod *v1.Pod) error {
	node, err := pl.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	allocCPU := node.Status.Allocatable.Cpu().MilliValue()
	allocMem := node.Status.Allocatable.Memory().Value()

	pods, err := pl.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return err
	}
	var usedCPU, usedMem int64
	for i := range pods.Items {
		usedCPU += getPodCPURequest(&pods.Items[i])
		usedMem += getPodMemoryRequest(&pods.Items[i])
	}
	needCPU := getPodCPURequest(pod)
	needMem := getPodMemoryRequest(pod)
	if usedCPU+needCPU > allocCPU || usedMem+needMem > allocMem {
		return fmt.Errorf("%w: would overcommit: used %dm/%dm + need %dm, used %d/%d + need %d",
			ErrNoRoom, usedCPU, allocCPU, needCPU, usedMem, allocMem, needMem)
	}
	return nil
}

func (pl *MyCrossNodePreemption) waitForPodGone(ctx context.Context, ns, name string, timeout time.Duration) error {
	// Poll every 300ms until the pod is NotFound
	return wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, timeout, true, func(ctx context.Context) (done bool, err error) {
		_, err = pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil // fully gone
		}
		return false, err // keep waiting (or bubble up unexpected errors)
	})
}

// hasController returns true if the pod has a controlling owner (e.g., ReplicaSet).
func hasController(p *v1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// recreatePendingCopy recreates a fresh Pending pod (same name) so it can be scheduled later.
func (pl *MyCrossNodePreemption) recreatePendingCopy(ctx context.Context, orig *v1.Pod) error {
	newp := orig.DeepCopy()
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.SchedulerName = "" // let normal scheduler bind it
	newp.Spec.NodeName = ""      // ensure it's Pending
	// DO NOT carry over any NodeSelector/Affinity that would pin it back where it was unless you want that:
	// newp.Spec.NodeSelector = map[string]string{}
	// newp.Spec.Affinity = nil

	// Optional breadcrumb:
	if newp.Annotations == nil {
		newp.Annotations = map[string]string{}
	}
	newp.Annotations["scheduler.alpha.kubernetes.io/evicted-by"] = Name
	newp.Annotations["scheduler.alpha.kubernetes.io/evicted-timestamp"] = time.Now().Format(time.RFC3339)

	// Keep the exact same name so it’s easy to track
	newp.GenerateName = ""
	newp.Name = orig.Name

	_, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{})
	return err
}
