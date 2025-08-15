package mycrossnodepreemption

import (
	"context"
	"fmt"
	"strconv"
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
		if freeCPU < 0 {
			freeCPU = 0
		}
		if freeMem < 0 {
			freeMem = 0
		}
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
				if sc := caps[mv.FromNode]; sc != nil {
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

	// deadlock breaker: pick a move that frees a blocked destination
	if len(remaining) > 0 {
		// Build set of currently-blocked destinations
		blockedDst := map[string]bool{}
		for _, mv := range remaining {
			dst := caps[mv.ToNode]
			needCPU, needMem := mv.CPURequest, mv.MemoryRequest
			if dst == nil || dst.freeCPU < needCPU || dst.freeMem < needMem {
				blockedDst[mv.ToNode] = true
			}
		}

		// Prefer a move whose FromNode is a blocked destination (frees space there)
		pickIdx := -1
		for i, mv := range remaining {
			if blockedDst[mv.FromNode] {
				pickIdx = i
				break
			}
		}
		// Fallback: free from the most overloaded node (by memory deficit, then CPU)
		if pickIdx == -1 {
			type deficit struct {
				idx      int
				mem, cpu int64
			}
			var best *deficit
			for i, mv := range remaining {
				src := caps[mv.FromNode]
				if src == nil {
					continue
				}
				d := &deficit{i, src.allocMem - src.freeMem, src.allocCPU - src.freeCPU}
				if best == nil || d.mem > best.mem || (d.mem == best.mem && d.cpu > best.cpu) {
					best = d
				}
			}
			if best != nil {
				pickIdx = best.idx
			}
		}

		mv := remaining[0]
		if pickIdx != -1 {
			mv = remaining[pickIdx]
		}

		klog.V(2).InfoS("Move deadlock detected; breaking by freeing bottleneck",
			"chosenPod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)

		// Perform the freeing move first (delete-first recreate on its destination)
		if err := pl.movePodToNodeDeleteFirst(ctx, mv.Pod, mv.ToNode); err != nil {
			moveFail++
			klog.ErrorS(err, "Deadlock break move failed", "pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
			return fmt.Errorf("failed to break deadlock by moving %s: %w", podRef(mv.Pod), err)
		}
		// Update ledgers
		if sc := caps[mv.FromNode]; sc != nil {
			sc.freeCPU += mv.CPURequest
			sc.freeMem += mv.MemoryRequest
		}
		if dc := caps[mv.ToNode]; dc != nil {
			dc.freeCPU -= mv.CPURequest
			dc.freeMem -= mv.MemoryRequest
		}
		moveOK++

		// Re-run greedy phase for the rest
		if pickIdx == -1 {
			// we used remaining[0]
			remaining = remaining[1:]
		} else {
			remaining = append(remaining[:pickIdx], remaining[pickIdx+1:]...)
		}
		subPlan := &PodAssignmentPlan{TargetNode: plan.TargetNode, PodMovements: remaining}
		return pl.executePlan(ctx, subPlan)
	}

	// ---- All moves done (no deadlock outstanding). Log a single summary and return. ----
	klog.InfoS("Plan execution summary", "targetNode", plan.TargetNode, "movesOK", moveOK, "movesFailed", moveFail, "evictionsOK", evictOK, "evictionsFailed", evictFail)
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
		// PropagationPolicy: &metav1.DeletePropagationForeground, // optional: ensure children first
	}); err != nil {
		return fmt.Errorf("delete original before move: %w", err)
	}

	// 2) Wait until it’s actually gone so we can reuse the same name
	//    (tune timeout if your cluster can take longer to GC)
	if err := pl.waitForPodGone(ctx, pod.Namespace, pod.Name, 30*time.Second); err != nil {
		return fmt.Errorf("wait for old pod to disappear: %w", err)
	}

	// 3) Recreate pinned at destination with the SAME name
	moved := pod.DeepCopy()
	moved.ResourceVersion = ""    // must be empty on create
	moved.UID = ""                // new object
	moved.Status = v1.PodStatus{} // clear runtime status
	// Don’t carry Node selectors/affinity from old placement (optional, your call)
	moved.Spec.NodeSelector = map[string]string{}
	moved.Spec.Affinity = nil
	moved.Spec.SchedulerName = "" // allow NodeName take effect
	moved.Spec.NodeName = destNode

	// Keep the EXACT same name
	moved.GenerateName = ""
	moved.Name = pod.Name

	// Movement annotations
	if moved.Annotations == nil {
		moved.Annotations = map[string]string{}
	}
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = pod.Spec.NodeName
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
	moved.Annotations["scheduler.alpha.kubernetes.io/moved-timestamp"] = time.Now().Format(time.RFC3339)
	if _, ok := moved.Annotations["scheduler.alpha.kubernetes.io/original-node"]; !ok {
		moved.Annotations["scheduler.alpha.kubernetes.io/original-node"] = pod.Spec.NodeName
	}
	moveCount, _ := strconv.Atoi(moved.Annotations["scheduler.alpha.kubernetes.io/move-count"])
	moved.Annotations["scheduler.alpha.kubernetes.io/move-count"] = fmt.Sprintf("%d", moveCount+1)

	// 4) Live capacity check before create
	if err := pl.verifyNodeHasRoomFor(ctx, destNode, moved); err != nil {
		return fmt.Errorf("destination check failed on %s for %s: %w", destNode, podRef(pod), err)
	}

	// 5) Create
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
		return fmt.Errorf("would overcommit: used %dm/%dm + need %dm, used %d/%d + need %d",
			usedCPU, allocCPU, needCPU, usedMem, allocMem, needMem)
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
