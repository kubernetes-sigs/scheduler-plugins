// executeplan.go
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

const (
	deletionCostTarget = -100
	deletionCostKeep   = 100

	waitControllerDeleteTimeout = 20 * time.Second
	waitControllerDeletePoll    = 300 * time.Millisecond
	deletionCostAnnotation      = "controller.kubernetes.io/pod-deletion-cost"
)

// See comments in previous message: standalone pods are recreated without binding,
// RS pods are recreated by their controllers, and the pending preemptor is bound.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan, pending *v1.Pod) error {
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

	rsDeltas := map[struct{ ns, name string }]int32{}

	if len(targets) > 0 {
		klog.V(2).InfoS("Setting deletion-cost annotations for targets and siblings", "targets", len(targets))
		if err := pl.annotateDeletionCosts(ctx, targets, deletionCostTarget, deletionCostKeep, rsDeltas); err != nil {
			return fmt.Errorf("annotate deletion-cost: %w", err)
		}
	}

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

	if len(targets) > 0 {
		klog.V(2).InfoS("Deleting/awaiting deletion of targeted pods", "count", len(targets))
		for _, pod := range targets {
			if _, isRS := owningReplicaSet(pod); isRS {
				if err := pl.waitPodGone(ctx, pod, waitControllerDeleteTimeout); err != nil {
					return fmt.Errorf("wait for RS pod deletion: %w", err)
				}
			} else {
				if err := pl.deleteAndWaitPodGone(ctx, pod, waitControllerDeleteTimeout); err != nil {
					return fmt.Errorf("delete non-RS pod: %w", err)
				}
			}
		}
	}

	// Bind the preemptor to the nominated node
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

	// Restore owner scales (lets controllers recreate RS pods)
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

	// Recreate standalone pods only, WITHOUT binding (Filter steers placement)
	for _, mv := range plan.PodMovements {
		if _, isRS := owningReplicaSet(mv.Pod); isRS {
			klog.V(2).InfoS("Skipping RS-owned move (controller will recreate)", "pod", podRef(mv.Pod), "to", mv.ToNode)
			continue
		}
		klog.V(2).InfoS("Recreating moved standalone pod (no bind)", "pod", podRef(mv.Pod))
		if err := pl.recreatePod(ctx, mv.Pod, ""); err != nil {
			return fmt.Errorf("recreate moved pod %s: %w", podRef(mv.Pod), err)
		}
	}
	for _, v := range plan.VictimsToEvict {
		if _, isRS := owningReplicaSet(v); isRS {
			klog.V(2).InfoS("Skipping RS-owned eviction recreate (controller will recreate)", "pod", podRef(v))
			continue
		}
		klog.V(2).InfoS("Recreating evicted standalone pod (no bind)", "pod", podRef(v))
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
			return false, nil
		}
		return false, nil
	})
}

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
	if err := pl.waitForPodGone(ctx, pod.Namespace, pod.Name, timeout); err != nil {
		return fmt.Errorf("wait for pod deletion: %w", err)
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

// ---------------------------- Deletion-cost helpers ----------------------------

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
		rs, err := pl.client.AppsV1().ReplicaSets(k.ns).Get(ctx, k.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get rs %s/%s: %w", k.ns, k.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return fmt.Errorf("selector for rs %s/%s: %w", k.ns, k.name, err)
		}
		podList, err := pl.client.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return fmt.Errorf("list rs pods %s/%s: %w", k.ns, k.name, err)
		}

		targetSet := map[string]struct{}{}
		for _, p := range pods {
			targetSet[p.Name] = struct{}{}
		}

		for _, p := range pods {
			if err := pl.setDeletionCost(ctx, p.Namespace, p.Name, targetCost); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("set deletion-cost target %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
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

// ---------------------------- Bind / Wait / Recreate ----------------------------

func (pl *MyCrossNodePreemption) bindPodToNode(ctx context.Context, pod *v1.Pod, node string) error {
	b := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		Target: v1.ObjectReference{Kind: "Node", Name: node},
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
		return pod.Spec.NodeName == node, nil
	})
}

// Recreate a standalone pod WITHOUT binding (Filter steers placement)
func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.SchedulerName = "" // default scheduler
	newp.Spec.NodeName = ""      // no direct binding
	newp.Spec.NodeSelector = map[string]string{}
	newp.Spec.Affinity = nil

	if newp.Annotations == nil {
		newp.Annotations = map[string]string{}
	}
	newp.Annotations["scheduler.alpha.kubernetes.io/previous-node"] = orig.Spec.NodeName
	newp.Annotations["scheduler.alpha.kubernetes.io/last-modified"] = time.Now().Format(time.RFC3339)

	newp.GenerateName = ""
	newp.Name = orig.Name

	if _, err := pl.client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	return nil
}
