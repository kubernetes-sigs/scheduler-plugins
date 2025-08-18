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

var ErrNoRoom = fmt.Errorf("destination has no room")

// executePlan:
//  1. Delete all pods that must move or evict.
//  2. Wait for the preemptor to bind to the solver's nominated node.
//  3. Recreate all moved pods on their destination nodes.
//  4. Recreate all evicted pods, without a target node.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan, pending *v1.Pod) error {
	var moveOK, moveFail int
	var evictionsFailed bool

	// 1) Delete all pods that will be moved or evicted
	if len(plan.PodMovements) > 0 {
		klog.V(2).InfoS("Deleting pods to be moved/evicted first", "count", len(plan.PodMovements)+len(plan.VictimsToEvict))
		var toDelete []*v1.Pod
		seen := map[string]bool{}
		for _, mv := range plan.PodMovements {
			key := mv.Pod.Namespace + "/" + mv.Pod.Name
			if !seen[key] {
				seen[key] = true
				toDelete = append(toDelete, mv.Pod)
			}
		}
		for _, v := range plan.VictimsToEvict {
			key := v.Namespace + "/" + v.Name
			if !seen[key] {
				seen[key] = true
				toDelete = append(toDelete, v)
			}
		}
		if err := pl.deletePodsWaitGone(ctx, toDelete); err != nil {
			evictionsFailed = true
			return fmt.Errorf("delete moved/evicted pods: %w", err)
		}
	}

	// 2) Wait for the preemptor to bind on the nominated node
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

	// 3) Recreate moved pods directly on their solver-chosen destination nodes
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("Recreating moved pod",
			"idx", fmt.Sprintf("%d/%d", i+1, len(plan.PodMovements)),
			"pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
		if err := pl.recreatePod(ctx, mv.Pod, mv.ToNode); err != nil {
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

	// 4) Recreate evicted pods, with no target node.
	for _, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Recreating evicted pod",
			"pod", podRef(v))
		if err := pl.recreatePod(ctx, v, ""); err != nil {
			// If a controller recreated it already, treat as success.
			if apierrors.IsAlreadyExists(err) {
				klog.V(3).InfoS("Evicted pod already exists after controller recreation; treating as success", "pod", podRef(v))
			} else {
				moveFail++
				return fmt.Errorf("recreate evicted pod %s: %w", podRef(v), err)
			}
		}
		moveOK++
	}

	klog.InfoS("Plan execution summary",
		"targetNode", plan.TargetNode,
		"movesOK", moveOK, "movesFailed", moveFail,
		"evictions", len(plan.VictimsToEvict), "evictionsFailed", evictionsFailed)

	if moveFail == 0 && !evictionsFailed {
		klog.InfoS("Plan executed successfully")
	} else {
		klog.ErrorS(nil, "Plan executed with failures",
			"movesFailed", moveFail, "evictionsFailed", evictionsFailed)
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

// recreatePod recreates a pod object pinned to destNode.
// It resets volatile fields and neutralizes NodeSelector/Affinity to avoid conflicts.
func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, destNode string) error {
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
	newp.Annotations["scheduler.alpha.kubernetes.io/previous-node"] = orig.Spec.NodeName
	newp.Annotations["scheduler.alpha.kubernetes.io/last-modified"] = time.Now().Format(time.RFC3339)

	// Keep exact same pod name for traceability
	newp.GenerateName = ""
	newp.Name = orig.Name

	_, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{})
	return err
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
