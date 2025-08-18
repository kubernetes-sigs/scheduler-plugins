package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// Strong deletion-cost bias values.
// Lower = more likely to be deleted when scaling down.
const (
	deletionCostTarget = -100
	deletionCostKeep   = 100

	// How long we wait for the controller to delete low-cost targets.
	waitControllerDeleteTimeout = 20 * time.Second
	waitControllerDeletePoll    = 300 * time.Millisecond
	deletionCostAnnotation      = "controller.kubernetes.io/pod-deletion-cost"
)

// executePlan (with deletion-cost bias + owner scale-down):
//  1. Mark chosen pods (to move/evict) with low deletion-cost; mark siblings with high cost.
//  2. Scale down owning Deployment (preferred) or ReplicaSet by targets-per-RS.
//  3. Wait for controller deletions / explicit deletion for non-RS pods.
//  4. Bind the preemptor to nominated node.
//  5. Recreate moved pods on destination nodes.
//  6. Recreate evicted pods without target node.
//  7. Restore scales reduced in step 2.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan, pending *v1.Pod) error {
	// Collect target pods (moves + evictions).
	var targets []*v1.Pod
	seen := map[string]bool{}
	for _, mv := range plan.PodMovements {
		key := mv.Pod.Namespace + "/" + mv.Pod.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, mv.Pod)
		}
	}
	for _, v := range plan.VictimsToEvict {
		key := v.Namespace + "/" + v.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, v)
		}
	}

	// Per-ReplicaSet deltas (negative for initial scale down).
	rsDeltas := map[struct{ ns, name string }]int32{}

	// Step 1: deletion-cost annotations
	if len(targets) > 0 {
		klog.V(2).InfoS("Setting deletion-cost annotations for targets and siblings", "targets", len(targets))
		if err := pl.annotateDeletionCosts(ctx, targets, deletionCostTarget, deletionCostKeep, rsDeltas); err != nil {
			return fmt.Errorf("annotate deletion-cost: %w", err)
		}
	}

	// Step 2: scale down Deployment/RS owners
	if len(rsDeltas) > 0 {
		klog.V(2).InfoS("Scaling down owners for targeted ReplicaSets", "sets", len(rsDeltas))
		for k, d := range rsDeltas {
			if d == 0 {
				continue
			}
			if err := pl.bumpOwnerScale(ctx, k.ns, k.name, d); err != nil {
				return fmt.Errorf("scale down %s/%s by %d: %w", k.ns, k.name, d, err)
			}
		}
	}

	// Step 3: wait for pod deletions
	if len(targets) > 0 {
		klog.V(2).InfoS("Deleting/awaiting deletion of targeted pods", "count", len(targets))
		for _, pod := range targets {
			// Check if rs-owned or not
			if _, isRS := owningReplicaSet(pod); isRS {
				// Wait for controller to delete (because we scaled down the owner).
				if err := pl.waitPodGone(ctx, pod, waitControllerDeleteTimeout); err != nil {
					return fmt.Errorf("wait for pod deletion: %w", err)
				}
			} else {
				// Non-RS pod: Evict first (respects PDB).
				if err := pl.deleteAndWaitPodGone(ctx, pod, waitControllerDeleteTimeout); err != nil {
					return fmt.Errorf("evict non-RS pod: %w", err)
				}
			}
		}
	}

	// Step 4: bind the preemptor (unchanged)
	if pending != nil && plan.TargetNode != "" {
		klog.V(2).InfoS("Binding preemptor to nominated node",
			"pod", podRef(pending), "targetNode", plan.TargetNode)
		if err := pl.bindPodToNode(ctx, pending, plan.TargetNode); err != nil {
			klog.ErrorS(err, "Direct bind attempt failed; will still wait/verify",
				"pod", podRef(pending), "targetNode", plan.TargetNode)
		}
		if err := pl.waitForPodBound(ctx, pending.Namespace, pending.Name, plan.TargetNode, 30*time.Second); err != nil {
			klog.ErrorS(err, "Preemptor failed to bind", "pod", podRef(pending), "targetNode", plan.TargetNode)
		}
	}

	// Step 7: restore owner scales
	if len(rsDeltas) > 0 {
		klog.V(2).InfoS("Restoring owner scales", "sets", len(rsDeltas))
		for k, d := range rsDeltas {
			if d == 0 {
				continue
			}
			if err := pl.bumpOwnerScale(ctx, k.ns, k.name, -d); err != nil {
				klog.ErrorS(err, "Failed to restore owner scale", "rs", fmt.Sprintf("%s/%s", k.ns, k.name), "delta", -d)
			}
		}
	}

	// Step 5: recreate moved pods on destination
	for i, mv := range plan.PodMovements {
		klog.V(2).InfoS("Recreating moved pod",
			"idx", fmt.Sprintf("%d/%d", i+1, len(plan.PodMovements)),
			"pod", podRef(mv.Pod), "from", mv.FromNode, "to", mv.ToNode)
		if err := pl.recreatePod(ctx, mv.Pod, mv.ToNode); err != nil {
			return fmt.Errorf("recreate moved pod %s on %s: %w", podRef(mv.Pod), mv.ToNode, err)
		}
	}

	// Step 6: recreate evicted pods (no target node)
	for _, v := range plan.VictimsToEvict {
		klog.V(2).InfoS("Recreating evicted pod", "pod", podRef(v))
		if err := pl.recreatePod(ctx, v, ""); err != nil {
			return fmt.Errorf("recreate evicted pod %s: %w", podRef(v), err)
		}
	}

	klog.InfoS("Plan execution summary",
		"targetNode", plan.TargetNode, "moves", len(plan.PodMovements),
		"evictions", len(plan.VictimsToEvict))

	return nil
}

// ---------------------------- Deletion helpers ----------------------------

func (pl *MyCrossNodePreemption) waitPodGone(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, waitControllerDeletePoll, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := pl.client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			// transient API error; keep polling
			return false, nil
		}
		return false, nil
	})
}

// deletePod:
// - If RS-owned: just wait (poll) for controller deletion; no raw delete.
// - If NOT RS-owned: try Eviction; if still present or eviction fails, force delete with UID preconditions; then wait gone.
func (pl *MyCrossNodePreemption) deleteAndWaitPodGone(
	ctx context.Context,
	pod *v1.Pod,
	timeout time.Duration,
) error {
	grace := int64(0)
	pre := &metav1.Preconditions{UID: &pod.UID}
	if derr := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		Preconditions:      pre,
	}); derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("delete pod %s: %w", podRef(pod), derr)
	}

	// Wait for deletion to complete.
	if err := pl.waitForPodGone(ctx, pod.Namespace, pod.Name, timeout); err != nil {
		return fmt.Errorf("wait for pod deletion: %w", err)
	}
	return nil
}

// ---------------------------- Deletion-cost helpers ----------------------------

// annotateDeletionCosts sets low deletion-cost on target pods and high deletion-cost on their
// RS siblings; it also fills rsDeltas with negative counts for initial scale-down.
func (pl *MyCrossNodePreemption) annotateDeletionCosts(
	ctx context.Context,
	targets []*v1.Pod,
	targetCost, siblingCost int,
	rsDeltas map[struct{ ns, name string }]int32,
) error {
	type key struct{ ns, name string }
	group := map[key][]*v1.Pod{}

	for _, p := range targets {
		if rsName, ok := owningReplicaSet(p); ok {
			k := key{ns: p.Namespace, name: rsName}
			group[k] = append(group[k], p)
			rsDeltas[k] -= 1
		}
	}

	for k, pods := range group {
		// Fetch RS and list its pods via selector
		rs, err := pl.client.AppsV1().ReplicaSets(k.ns).Get(ctx, k.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get rs %s/%s: %w", k.ns, k.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return fmt.Errorf("selector for rs %s/%s: %w", k.ns, k.name, err)
		}
		podList, err := pl.client.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return fmt.Errorf("list rs pods %s/%s: %w", k.ns, k.name, err)
		}

		targetSet := map[string]struct{}{}
		for _, p := range pods {
			targetSet[p.Name] = struct{}{}
		}

		// Low cost on targets
		for _, p := range pods {
			if err := pl.setDeletionCost(ctx, p.Namespace, p.Name, targetCost); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("set deletion-cost target %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
		// High cost on siblings owned by this RS
		for i := range podList.Items {
			sib := &podList.Items[i]
			if !isControlledByRS(sib, k.name) {
				continue
			}
			if _, isTarget := targetSet[sib.Name]; isTarget {
				continue
			}
			if err := pl.setDeletionCost(ctx, sib.Namespace, sib.Name, siblingCost); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("set deletion-cost keep %s/%s: %w", sib.Namespace, sib.Name, err)
			}
		}
	}
	return nil
}

func (pl *MyCrossNodePreemption) setDeletionCost(ctx context.Context, ns, podName string, cost int) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%d"}}}`, deletionCostAnnotation, cost))
	_, err := pl.client.CoreV1().Pods(ns).Patch(ctx, podName, types.StrategicMergePatchType, patch, metav1.PatchOptions{
		FieldManager: "my-crossnode-plugin",
	})
	return err
}

// ---------------------------- Owner scale helpers ----------------------------

// bumpOwnerScale updates the scale of the owning Deployment if present, else the RS.
// delta is typically negative for initial scale-down, positive for restore.
func (pl *MyCrossNodePreemption) bumpOwnerScale(ctx context.Context, ns, rsName string, delta int32) error {
	rs, err := pl.client.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var depName string
	for _, o := range rs.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "Deployment" {
			depName = o.Name
			break
		}
	}
	if depName != "" {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			sc, err := pl.client.AppsV1().Deployments(ns).GetScale(ctx, depName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			sc.Spec.Replicas += delta
			_, err = pl.client.AppsV1().Deployments(ns).UpdateScale(ctx, depName, sc, metav1.UpdateOptions{})
			return err
		})
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sc, err := pl.client.AppsV1().ReplicaSets(ns).GetScale(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		sc.Spec.Replicas += delta
		_, err = pl.client.AppsV1().ReplicaSets(ns).UpdateScale(ctx, rsName, sc, metav1.UpdateOptions{})
		return err
	})
}

// ---------------------------- Bind / Wait / Recreate (your originals) ----------------------------

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
	if err := pl.client.CoreV1().Pods(pod.Namespace).Bind(ctx, b, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("bind pod %s to node %s: %w", podRef(pod), node, err)
	}
	if err := pl.waitForPodBound(ctx, pod.Namespace, pod.Name, node, 30*time.Second); err != nil {
		klog.ErrorS(err, "Failed to bind pod to node", "pod", podRef(pod), "node", node)
		return fmt.Errorf("wait for pod %s to be bound to node %s: %w", podRef(pod), node, err)
	}
	return nil
}

func (pl *MyCrossNodePreemption) waitForPodBound(ctx context.Context, ns, name, node string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (done bool, err error) {
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

// recreatePod recreates a pod object pinned to destNode.
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
	newp.Annotations[deletionCostAnnotation] = fmt.Sprintf("%d", deletionCostKeep) // set high to prevent replicationController take our created pods after increasing replica count

	newp.GenerateName = ""
	newp.Name = orig.Name

	if _, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	// Bind the new pod to the destination node
	if err := pl.waitForPodBound(ctx, newp.Namespace, newp.Name, destNode, 30*time.Second); err != nil {
		klog.ErrorS(err, "Failed to bind pod to node", "pod", podRef(newp), "node", destNode)
		return fmt.Errorf("wait for pod %s to be bound to node %s: %w", podRef(newp), destNode, err)
	}
	return nil
}

func (pl *MyCrossNodePreemption) waitForPodGone(ctx context.Context, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, timeout, true, func(ctx context.Context) (done bool, err error) {
		_, err = pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// ---------------------------- Small owner helpers ----------------------------

func owningReplicaSet(p *v1.Pod) (string, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" {
			return o.Name, true
		}
	}
	return "", false
}

func isControlledByRS(p *v1.Pod, rsName string) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" && o.Name == rsName {
			return true
		}
	}
	return false
}
