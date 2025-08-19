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
	DeletionCostTarget = -100
	DeletionCostKeep   = 100

	DeleteTimeout     = 10 * time.Second
	DeletePollTimeout = 1 * time.Second
)

// prevDeletionCosts maps "ns/name" -> original value pointer.
// nil => annotation absent; non-nil => original string value.
type prevDeletionCosts map[string]*string

// Standalone pods are recreated without binding (Filter steers placement).
// RS pods are recreated by their controllers after scale restore.
// Pending pod is *not* bound here; Filter constrains it to the target node
// and the default scheduler performs the bind.
func (pl *MyCrossNodePreemption) executePlan(ctx context.Context, plan *PodAssignmentPlan, pending *v1.Pod) error {
	// Collect unique target pods (moves + evictions).
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

	// 1) deletion-cost annotations (remember previous values)
	var prevCosts prevDeletionCosts
	if len(targets) > 0 {
		klog.V(2).InfoS("Setting deletion-cost annotations for targets and siblings", "targets", len(targets))
		var err error
		prevCosts, err = pl.annotateDeletionCosts(ctx, targets, DeletionCostTarget, DeletionCostKeep, rsDeltas)
		if err != nil {
			return fmt.Errorf("annotate deletion-cost: %w", err)
		}
	}

	// 2) scale down owners
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

	// 3) wait/delete pods
	if len(targets) > 0 {
		klog.V(2).InfoS("Deleting/awaiting deletion of targeted pods", "count", len(targets))
		for _, pod := range targets {
			// RS pods are deleted by their controllers
			if _, isRS := owningReplicaSet(pod); isRS {
				if err := pl.waitPodGone(ctx, pod, DeleteTimeout); err != nil {
					return fmt.Errorf("wait for RS pod deletion %s: %w", podRef(pod), err)
				}
			} else { // standalone pod, will be deleted directly here
				if err := pl.deletePod(ctx, pod); err != nil {
					return fmt.Errorf("delete non-RS pod %s: %w", podRef(pod), err)
				}
				if err := pl.waitPodGone(ctx, pod, DeleteTimeout); err != nil {
					return fmt.Errorf("wait for non-RS pod deletion %s: %w", podRef(pod), err)
				}
			}
		}
	}

	// DO NOT bind pending pod here — Filter will gate it to target node.

	// 4) restore owner scales (no bind)
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
		// Wait for RS-owned pods to be
	}

	// 5) recreate standalone pods (no bind)
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

	// 6) restore original deletion-costs
	if len(prevCosts) > 0 {
		klog.V(2).InfoS("Restoring previous pod-deletion-cost annotations", "count", len(prevCosts))
		pl.restoreDeletionCosts(ctx, prevCosts)
	}

	return nil
}

// ---------------------------- Deletion helpers ----------------------------

func (pl *MyCrossNodePreemption) waitPodGone(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, DeletePollTimeout, timeout, true, func(ctx context.Context) (bool, error) {
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

func (pl *MyCrossNodePreemption) deletePod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
	pre := &metav1.Preconditions{UID: &pod.UID}
	if derr := pl.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		Preconditions:      pre,
	}); derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("delete pod %s: %w", podRef(pod), derr)
	}
	return nil
}

// ---------------------------- Deletion-cost helpers ----------------------------

// annotateDeletionCosts sets low deletion-cost on target pods and high deletion-cost on their
// RS siblings; it also fills rsDeltas with negative counts for initial scale-down.
// It RETURNS the *previous* values of the annotation for every pod it touched.
func (pl *MyCrossNodePreemption) annotateDeletionCosts(
	ctx context.Context,
	targets []*v1.Pod,
	targetCost, siblingCost int,
	rsDeltas map[struct{ ns, name string }]int32,
) (prevDeletionCosts, error) {
	type key struct{ ns, name string }
	group := map[key][]*v1.Pod{}
	prev := prevDeletionCosts{}

	// Group targets per RS and populate rsDeltas
	for _, p := range targets {
		if rsName, ok := owningReplicaSet(p); ok {
			k := key{ns: p.Namespace, name: rsName}
			group[k] = append(group[k], p)
			rsDeltas[k] -= 1
		}
	}

	// For each RS: set target (low) and sibling (high); record previous for all touched
	for k, pods := range group {
		rs, err := pl.client.AppsV1().ReplicaSets(k.ns).Get(ctx, k.name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get rs %s/%s: %w", k.ns, k.name, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("selector for rs %s/%s: %w", k.ns, k.name, err)
		}
		podList, err := pl.client.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, fmt.Errorf("list rs pods %s/%s: %w", k.ns, k.name, err)
		}

		targetSet := map[string]struct{}{}
		for _, p := range pods {
			targetSet[p.Name] = struct{}{}
		}

		// Low cost on targets — record previous and patch
		for _, p := range pods {
			key := p.Namespace + "/" + p.Name
			if _, seen := prev[key]; !seen {
				if p.Annotations != nil {
					if v, ok := p.Annotations[DeletionCostAnnotation]; ok {
						vv := v
						prev[key] = &vv
					} else {
						prev[key] = nil
					}
				} else {
					prev[key] = nil
				}
			}
			if err := pl.setDeletionCost(ctx, p.Namespace, p.Name, targetCost); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("set deletion-cost target %s/%s: %w", p.Namespace, p.Name, err)
			}
		}

		// High cost on siblings — record previous and patch
		for i := range podList.Items {
			sib := &podList.Items[i]
			if !isControlledByRS(sib, k.name) {
				continue
			}
			if _, isTarget := targetSet[sib.Name]; isTarget {
				continue
			}
			key := sib.Namespace + "/" + sib.Name
			if _, seen := prev[key]; !seen {
				if sib.Annotations != nil {
					if v, ok := sib.Annotations[DeletionCostAnnotation]; ok {
						vv := v
						prev[key] = &vv
					} else {
						prev[key] = nil
					}
				} else {
					prev[key] = nil
				}
			}
			if err := pl.setDeletionCost(ctx, sib.Namespace, sib.Name, siblingCost); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("set deletion-cost keep %s/%s: %w", sib.Namespace, sib.Name, err)
			}
		}
	}
	return prev, nil
}

func (pl *MyCrossNodePreemption) setDeletionCost(ctx context.Context, ns, podName string, cost int) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%d"}}}`, DeletionCostAnnotation, cost))
	_, err := pl.client.CoreV1().Pods(ns).Patch(ctx, podName, types.StrategicMergePatchType, patch, metav1.PatchOptions{
		FieldManager: "my-crossnode-plugin",
	})
	return err
}

// restoreDeletionCosts tries to put back each pod's original value.
// If prev is nil => delete the annotation. If not found => ignore.
func (pl *MyCrossNodePreemption) restoreDeletionCosts(ctx context.Context, prev prevDeletionCosts) {
	for key, old := range prev {
		ns, name := splitNSName(key) // key is "ns/name"
		var patch []byte
		if old == nil {
			// remove annotation
			patch = []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, DeletionCostAnnotation))
		} else {
			// restore exact prior value
			patch = []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, DeletionCostAnnotation, *old))
		}
		if _, err := pl.client.CoreV1().Pods(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{
			FieldManager: "my-crossnode-plugin",
		}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to restore deletion-cost", "pod", key)
		} else {
			klog.V(3).InfoS("Restored deletion-cost", "pod", key)
		}
	}
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
