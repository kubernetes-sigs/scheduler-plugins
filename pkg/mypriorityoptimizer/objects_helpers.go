// objects_helpers.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// -----------------------------------------------------------------------------
// Test Hooks
// -----------------------------------------------------------------------------

var (
	nodesListerFor = func(pl *SharedState) corev1listers.NodeLister {
		return pl.Handle.SharedInformerFactory().Core().V1().Nodes().Lister()
	}
	podsListerFor = func(pl *SharedState) corev1listers.PodLister {
		return pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	}
	evictPodFor = func(pl *SharedState, ctx context.Context, pod *v1.Pod, ev *policyv1.Eviction) error {
		return pl.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
	}
)

// nodesLister returns the NodeLister from the shared informer factory.
func (pl *SharedState) nodesLister() corev1listers.NodeLister {
	return nodesListerFor(pl)
}

// podsLister returns the PodsLister from the shared informer factory.
func (pl *SharedState) podsLister() corev1listers.PodLister {
	return podsListerFor(pl)
}

// getNodes returns a list of all nodes in the cluster.
// Use the informer lister to avoid stale data from SnapshotLister.
func (pl *SharedState) getNodes() ([]*v1.Node, error) {
	return pl.nodesLister().List(labels.Everything())
}

// getPods returns a list of all pods in the cluster.
// Use the informer lister to avoid stale data from SnapshotLister.
func (pl *SharedState) getPods() ([]*v1.Pod, error) {
	return pl.podsLister().List(labels.Everything())
}

// podRef returns a string representation of the pod's namespace and name.
func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

// mergeNsName combines namespace and name into a single string with a '/' separator.
func mergeNsName(ns, name string) string {
	return fmt.Sprintf("%s/%s", ns, name)
}

// splitNsName splits a combined namespace/name string into its components.
func splitNsName(nsname string) (string, string, error) {
	parts := strings.SplitN(nsname, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid namespace/name format: %q", nsname)
	}
	return parts[0], parts[1], nil
}

// countPendingPods returns the number of pods that are currently pending (alive and unbound).
func countPendingPods(pods []*v1.Pod) int {
	if len(pods) == 0 {
		return 0
	}
	n := 0
	for _, p := range pods {
		if isPodDeleted(p) {
			continue
		}
		if !isPodAssigned(p) {
			n++
		}
	}
	return n
}

// evictPod evicts a pod from the cluster using the eviction API.
// grace is set to 0 for immediate eviction.
func (pl *SharedState) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0) // immediate eviction
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		},
	}
	return evictPodFor(pl, ctx, pod, ev)
}

// getNodeCPUAllocatable returns the allocatable CPU of a node in millicores.
func getNodeCPUAllocatable(n *v1.Node) int64 {
	return n.Status.Allocatable.Cpu().MilliValue()
}

// getNodeMemoryAllocatable returns the allocatable memory of a node in bytes.
func getNodeMemoryAllocatable(n *v1.Node) int64 {
	return n.Status.Allocatable.Memory().Value()
}

// isNodeControlPlane returns true if the node is a control plane node.
// Additional labels can be added here as needed.
func isNodeControlPlane(n *v1.Node) bool {
	return n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
}

// getNodeConditions returns the conditions of a node.
func getNodeConditions(n *v1.Node) []v1.NodeCondition {
	return n.Status.Conditions
}

// isNodeReady returns true if the node is ready.
func isNodeReady(n *v1.Node) bool {
	conditions := getNodeConditions(n)
	for _, c := range conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

// getNodeTaints returns the taints of a node.
func getNodeTaints(n *v1.Node) []v1.Taint {
	return n.Spec.Taints
}

// isNodeNoScheduleConditionTainted returns true if the node has a NoSchedule taint due to not ready or unreachable conditions.
func isNodeNoScheduleConditionTainted(n *v1.Node) bool {
	taints := getNodeTaints(n)
	for _, t := range taints {
		if (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") &&
			(t.Effect == v1.TaintEffectNoSchedule || string(t.Effect) == "") {
			return true
		}
	}
	return false
}

// isNodeAllocatable returns true if the node has allocatable CPU and memory resources.
func isNodeAllocatable(n *v1.Node) bool {
	return getNodeCPUAllocatable(n) > 0 && getNodeMemoryAllocatable(n) > 0
}

func isNodeUnschedulable(n *v1.Node) bool {
	return n.Spec.Unschedulable
}

// isNodeUsable returns true if the node is usable for scheduling.
func isNodeUsable(n *v1.Node) bool {
	return n != nil &&
		!isNodeControlPlane(n) &&
		!isNodeUnschedulable(n) &&
		isNodeReady(n) &&
		!isNodeNoScheduleConditionTainted(n) &&
		isNodeAllocatable(n)
}

// getPod attempts to find the pod by matching UID and name,
// using getPodByName first, then falling back to getPodByUID.
// Returns nil if the pod cannot be found or errors occur.
func (pl *SharedState) getPod(uid types.UID, ns, name string) *v1.Pod {
	// Fast path: lookup by namespace/name and verify UID.
	if p, err := pl.getPodByName(ns, name); err == nil && p != nil && isSamePodUID(p.UID, uid) {
		return p
	}

	// Fallback: lookup by UID across all pods (handles renames, stale name → UID).
	if p, err := pl.getPodByUID(uid); err == nil && p != nil {
		return p
	}

	return nil
}

// getPodByName returns the pod by namespace and name.
func (pl *SharedState) getPodByName(ns, name string) (*v1.Pod, error) {
	return pl.podsLister().Pods(ns).Get(name)
}

// getPodByUID returns the pod by UID by scanning all pods.
func (pl *SharedState) getPodByUID(uid types.UID) (*v1.Pod, error) {
	pods, err := pl.podsLister().List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		if isSamePodUID(p.UID, uid) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pod with UID %s not found", uid)
}

// getPodContainers returns all containers of a pod
func getPodContainers(p *v1.Pod) []v1.Container {
	return p.Spec.Containers
}

// getContainerCPURequest returns the CPU request of a container in millicores.
func getContainerCPURequest(c v1.Container) int64 {
	return c.Resources.Requests.Cpu().MilliValue()
}

// getContainerMemoryRequest returns the memory request of a container in bytes.
func getContainerMemoryRequest(c v1.Container) int64 {
	return c.Resources.Requests.Memory().Value()
}

// getPodCPURequest returns the total CPU request for a pod by summing the requests of all containers.
func getPodCPURequest(p *v1.Pod) int64 {
	containers := getPodContainers(p)
	var total int64
	for _, c := range containers {
		total += getContainerCPURequest(c)
	}
	return total
}

// getPodMemoryRequest returns the total memory request for a pod by summing the requests of all containers.
func getPodMemoryRequest(p *v1.Pod) int64 {
	containers := getPodContainers(p)
	var total int64
	for _, c := range containers {
		total += getContainerMemoryRequest(c)
	}
	return total
}

// getPodPriority returns the priority of a pod.
func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}

// getPodNodeName returns the name of the node to which the pod is assigned.
func getPodAssignedNodeName(p *v1.Pod) string {
	return p.Spec.NodeName
}

// isSamePodUID
func isSamePodUID(pod1, pod2 types.UID) bool {
	return string(pod1) != "" && string(pod2) != "" && pod1 == pod2
}

// isPodDeleted returns true if the pod has a deletion timestamp set.
func isPodDeleted(p *v1.Pod) bool {
	return p == nil || p.DeletionTimestamp != nil
}

// isPodAssigned returns true if the pod is assigned to a node.
func isPodAssigned(p *v1.Pod) bool {
	return p != nil && getPodAssignedNodeName(p) != ""
}

// isPodAssignedAndAlive returns true if the pod is non-nil, not terminating,
// and currently bound to a node. This is the set of pods we can count as "running now".
func isPodAssignedAndAlive(p *v1.Pod) bool {
	return !isPodDeleted(p) && isPodAssigned(p)
}

// isPodProtected returns true if the pod e.g. is a system pod and should not be evicted.
func isPodProtected(p *v1.Pod) bool {
	if p == nil {
		return false
	}
	// Example: protect pods in kube-system namespace
	if p.Namespace == SystemNamespace {
		return true
	}
	return false
}

// podsByUID returns a map of pod UIDs to their corresponding Pod objects.
func podsByUID(pods []*v1.Pod) map[types.UID]*v1.Pod {
	m := make(map[types.UID]*v1.Pod, len(pods))
	for _, p := range pods {
		if isPodDeleted(p) {
			continue
		}
		m[p.UID] = p
	}
	return m
}

// WorkloadKind represents the type of workload.
func (wk WorkloadKey) String() string {
	switch wk.Kind {
	case wkReplicaSet:
		return "rs:" + mergeNsName(wk.Namespace, wk.Name)
	case wkStatefulSet:
		return "ss:" + mergeNsName(wk.Namespace, wk.Name)
	case wkDaemonSet:
		return "ds:" + mergeNsName(wk.Namespace, wk.Name)
	case wkJob:
		return "job:" + mergeNsName(wk.Namespace, wk.Name)
	default:
		return mergeNsName(wk.Namespace, wk.Name)
	}
}

// getTopWorkload returns the top-level workload controller of a pod, if any.
func getTopWorkload(p *v1.Pod) (WorkloadKey, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		switch o.Kind {
		case "ReplicaSet":
			return WorkloadKey{Kind: wkReplicaSet, Namespace: p.Namespace, Name: o.Name}, true
		case "StatefulSet":
			return WorkloadKey{Kind: wkStatefulSet, Namespace: p.Namespace, Name: o.Name}, true
		case "DaemonSet":
			return WorkloadKey{Kind: wkDaemonSet, Namespace: p.Namespace, Name: o.Name}, true
		case "Job":
			return WorkloadKey{Kind: wkJob, Namespace: p.Namespace, Name: o.Name}, true
		}
	}
	return WorkloadKey{}, false
}
