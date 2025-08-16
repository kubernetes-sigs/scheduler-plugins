package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// executePlan:
//  1. Delete all pods that must MOVE.
//  2. Delete all EVICTIONS (for standalone victims, recreate a Pending copy).
//  3. Wait for the preemptor to bind to the solver's nominated node.
//  4. Recreate all moved pods on their final destination nodes.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan, pending *v1.Pod) error {
	var moveOK, moveFail, evictOK, evictFail int

	// 1) Delete all pods that will be moved
	if len(plan.PodMovements) > 0 {
		klog.V(2).InfoS("Deleting pods to be moved", "count", len(plan.PodMovements))
		var toDelete []*v1.Pod
		seen := map[string]bool{}
		for _, mv := range plan.PodMovements {
			key := mv.Pod.Namespace + "/" + mv.Pod.Name
			if !seen[key] {
				seen[key] = true
				toDelete = append(toDelete, mv.Pod)
			}
		}
		if err := pl.deletePodsWaitGone(ctx, toDelete); err != nil {
			return fmt.Errorf("delete moved pods: %w", err)
		}
	}

	// 2) Apply evictions (delete), and if naked pod, recreate a copy (when space becomes available it would eventually be scheduled)
	if len(plan.VictimsToEvict) > 0 {
		klog.V(2).InfoS("Evicting victims", "count", len(plan.VictimsToEvict))
		for i, v := range plan.VictimsToEvict {
			klog.V(2).InfoS("Evicting victim",
				"idx", fmt.Sprintf("%d/%d", i+1, len(plan.VictimsToEvict)),
				"pod", podRef(v), "node", v.Spec.NodeName)
			if err := pl.evictPod(ctx, v); err != nil {
				klog.ErrorS(err, "Eviction failed", "pod", podRef(v))
				evictFail++
			} else {
				evictOK++
			}
		}
	}

	// 3) Wait for the preemptor to bind on the nominated node
	if pending != nil && plan.TargetNode != "" {
		klog.V(2).InfoS("Binding preemptor to nominated node",
			"pod", podRef(pending), "targetNode", plan.TargetNode)

		// Best-effort bind (idempotent across retries thanks to UID)
		if err := pl.bindPodToNode(ctx, pending, plan.TargetNode); err != nil {
			// If the pod was already bound or disappears, we’ll detect below.
			klog.ErrorS(err, "Direct bind attempt failed; will still wait/verify",
				"pod", podRef(pending), "targetNode", plan.TargetNode)
		}

		// A small delay before checking the binding status
		time.Sleep(2000 * time.Millisecond)
		if err := pl.waitForPodBound(ctx, pending.Namespace, pending.Name, plan.TargetNode, 30*time.Second); err != nil {
			klog.ErrorS(err, "Preemptor failed to bind", "pod", podRef(pending), "targetNode", plan.TargetNode)
		}
	}

	// 4) Recreate moved pods directly on their solver-chosen destination nodes
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("Recreating moved pod",
			"idx", fmt.Sprintf("%d/%d", i+1, len(plan.PodMovements)),
			"pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
		if err := pl.recreateMovedPodOn(ctx, mv.Pod, mv.ToNode); err != nil {
			// If a controller recreated it already, treat as success.
			if apierrors.IsAlreadyExists(err) {
				klog.V(3).InfoS("Moved pod already exists after controller recreation; treating as success", "pod", podRef(mv.Pod))
			} else {
				moveFail++
				return fmt.Errorf("recreate moved pod %s on %s: %w", podRef(mv.Pod), mv.ToNode, err)
			}
		}
		moveOK++
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

// bindPodToNode performs a direct Bind to the given node.
// It’s safe to call even if the scheduler would eventually bind; this just removes the race.
func (pl *MyCrossNodePreemption) bindPodToNode(ctx context.Context, pod *v1.Pod, node string) error {
	b := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID, // protects from binding a different generation
			ResourceVersion: "",      // not required for Binding
		},
		Target: v1.ObjectReference{
			Kind: "Node",
			Name: node,
		},
	}
	return pl.client.CoreV1().Pods(pod.Namespace).Bind(ctx, b, metav1.CreateOptions{})
}

func (pl *MyCrossNodePreemption) waitForPodBound(ctx context.Context, ns, name, node string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 5000*time.Millisecond, timeout, true, func(ctx context.Context) (done bool, err error) {
		pod, err := pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, err
		}
		if err != nil {
			return false, err
		}
		if pod.Spec.NodeName == node {
			return true, nil
		}
		return false, nil
	})
}

// deletePodsWaitGone deletes pods (grace 0) and waits until each disappears.
func (pl *MyCrossNodePreemption) deletePodsWaitGone(ctx context.Context, pods []*v1.Pod) error {
	grace := int64(0)
	for _, p := range pods {
		if err := pl.client.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", podRef(p), err)
		}
	}
	// Wait for all gone
	for _, p := range pods {
		if err := pl.waitForPodGone(ctx, p.Namespace, p.Name, 30*time.Second); err != nil {
			return fmt.Errorf("wait gone %s: %w", podRef(p), err)
		}
	}
	return nil
}

// recreateMovedPodOn recreates a pod object pinned to destNode.
// It resets volatile fields and neutralizes NodeSelector/Affinity to avoid conflicts.
func (pl *MyCrossNodePreemption) recreateMovedPodOn(ctx context.Context, orig *v1.Pod, destNode string) error {
	newp := orig.DeepCopy()
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.SchedulerName = ""
	newp.Spec.NodeName = destNode
	newp.Spec.NodeSelector = map[string]string{}
	newp.Spec.Affinity = nil

	if newp.Annotations == nil {
		newp.Annotations = map[string]string{}
	}
	newp.Annotations["scheduler.alpha.kubernetes.io/moved-from"] = orig.Spec.NodeName
	newp.Annotations["scheduler.alpha.kubernetes.io/moved-to"] = destNode
	newp.Annotations["scheduler.alpha.kubernetes.io/moved-timestamp"] = time.Now().Format(time.RFC3339)

	// Keep exact same pod name for traceability
	newp.GenerateName = ""
	newp.Name = orig.Name

	_, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{})
	return err
}

// evictPod deletes a victim pod; if naked (no controller), recreate a Pending copy
// so it can be scheduled later when capacity appears.
func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	klog.V(4).InfoS("Evicting victim pod", "pod", klog.KObj(pod))

	grace := int64(0)
	if err := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil && !apierrors.IsNotFound(err) {
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

// waitForPodGone polls until the pod disappears (NotFound) or times out.
func (pl *MyCrossNodePreemption) waitForPodGone(ctx context.Context, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, timeout, true, func(ctx context.Context) (done bool, err error) {
		_, err = pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
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
	newp.Spec.SchedulerName = ""
	newp.Spec.NodeName = ""

	if newp.Annotations == nil {
		newp.Annotations = map[string]string{}
	}
	newp.Annotations["scheduler.alpha.kubernetes.io/evicted-by"] = Name
	newp.Annotations["scheduler.alpha.kubernetes.io/evicted-timestamp"] = time.Now().Format(time.RFC3339)

	newp.GenerateName = ""
	newp.Name = orig.Name

	_, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{})
	return err
}
