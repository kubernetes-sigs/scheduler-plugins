// objects_helpers.go

package mypriorityoptimizer

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// nodesLister returns the NodeLister from the shared informer factory.
func (pl *SharedState) nodesLister() corev1listers.NodeLister {
	return pl.Handle.SharedInformerFactory().Core().V1().Nodes().Lister()
}

// podsLister returns the PodsLister from the shared informer factory.
func (pl *SharedState) podsLister() corev1listers.PodLister {
	return pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
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

// combineNsName combines namespace and name into a single string with a '/' separator.
func combineNsName(ns, name string) string {
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
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName == "" {
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
	return pl.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev)
}

// recreateStandalonePod creates a new pod with the same specifications as the original pod.
// Needed for standalone pods as when they are evicted, they will not be recreated as they have no controllers.
// UID, GenerateName, ResourceVersion, NodeName, NodeSelector are all set to none.
func (pl *SharedState) recreateStandalonePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newPod := orig.DeepCopy()
	newPod.UID = ""
	newPod.GenerateName = ""
	newPod.ResourceVersion = ""
	newPod.Status = v1.PodStatus{}
	newPod.Spec.NodeName = "" // no direct binding
	newPod.Spec.NodeSelector = map[string]string{}
	if _, err := pl.Client.CoreV1().Pods(orig.Namespace).Create(ctx, newPod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newPod), err)
	}
	return nil
}

// getPodCPURequest returns the total CPU request for a pod by summing the requests of all containers.
func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Cpu().MilliValue()
	}
	return total
}

// getPodMemoryRequest returns the total memory request for a pod by summing the requests of all containers.
func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
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

// isControlPlane returns true if the node is a control plane node.
// Additional labels can be added here as needed.
func isControlPlane(n *v1.Node) bool {
	return n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
}

// nodeReady returns true if the node is ready.
func nodeReady(n *v1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}

// hasNoScheduleCondTaint returns true if the node has a NoSchedule taint due to not ready or unreachable conditions.
func hasNoScheduleCondTaint(n *v1.Node) bool {
	for _, t := range n.Spec.Taints {
		if (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") &&
			(t.Effect == v1.TaintEffectNoSchedule || string(t.Effect) == "") {
			return true
		}
	}
	return false
}

// isNodeUsable returns true if the node is usable for scheduling.
func isNodeUsable(n *v1.Node) bool {
	if n == nil || isControlPlane(n) || n.Spec.Unschedulable {
		return false
	}
	if !nodeReady(n) {
		return false
	}
	if hasNoScheduleCondTaint(n) {
		return false
	}
	return n.Status.Allocatable.Cpu().MilliValue() > 0 &&
		n.Status.Allocatable.Memory().Value() > 0
}

// podsByUID returns a map of pod UIDs to their corresponding Pod objects.
func podsByUID(pods []*v1.Pod) map[types.UID]*v1.Pod {
	m := make(map[types.UID]*v1.Pod, len(pods))
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		m[p.UID] = p
	}
	return m
}

// clusterFingerprint builds a stable hash of the "cluster state" that matters
// for the solver baseline:
//
//   - all usable nodes (name + allocatable CPU/MEM)
//   - all RUNNING (non-terminating) pods bound to usable nodes
//     (UID + node + CPU/MEM + priority)
//
// Pending pods are explicitly *not* included here, since we track them via the
// pending UID set separately. We only use this to decide whether the cluster
// is "the same" baseline for a previously-solved pending set.
//
// The fingerprint is cheap to compute for small clusters and stable across
// map-iteration nondeterminism thanks to sorting.
func clusterFingerprint(nodes []*v1.Node, pods []*v1.Pod) string {
	h := fnv.New64a()

	// Filter usable nodes and sort them by name for determinism.
	usable := make([]*v1.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if isNodeUsable(n) {
			usable = append(usable, n)
		}
	}
	sort.Slice(usable, func(i, j int) bool {
		return usable[i].Name < usable[j].Name
	})

	// Node capacities.
	for _, n := range usable {
		cpu := n.Status.Allocatable.Cpu().MilliValue()
		mem := n.Status.Allocatable.Memory().Value()
		_, _ = h.Write([]byte("N:"))
		_, _ = h.Write([]byte(n.Name))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(strconv.FormatInt(cpu, 10)))
		_, _ = h.Write([]byte("/"))
		_, _ = h.Write([]byte(strconv.FormatInt(mem, 10)))
		_, _ = h.Write([]byte(";"))
	}

	usableNames := make(map[string]struct{}, len(usable))
	for _, n := range usable {
		usableNames[n.Name] = struct{}{}
	}

	// Collect running pods on usable nodes and sort (node, UID) for determinism.
	type podKey struct {
		node string
		uid  types.UID
	}
	keys := make([]podKey, 0, len(pods))
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName == "" {
			continue // pending
		}
		if _, ok := usableNames[p.Spec.NodeName]; !ok {
			continue
		}
		keys = append(keys, podKey{node: p.Spec.NodeName, uid: p.UID})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].node != keys[j].node {
			return keys[i].node < keys[j].node
		}
		return keys[i].uid < keys[j].uid
	})

	byUID := podsByUID(pods)

	for _, k := range keys {
		p := byUID[k.uid]
		if p == nil {
			continue
		}
		cpu := getPodCPURequest(p)
		mem := getPodMemoryRequest(p)
		prio := getPodPriority(p)

		_, _ = h.Write([]byte("P:"))
		_, _ = h.Write([]byte(string(p.UID)))
		_, _ = h.Write([]byte("@"))
		_, _ = h.Write([]byte(p.Spec.NodeName))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(strconv.FormatInt(cpu, 10)))
		_, _ = h.Write([]byte("/"))
		_, _ = h.Write([]byte(strconv.FormatInt(mem, 10)))
		_, _ = h.Write([]byte("#"))
		_, _ = h.Write([]byte(strconv.Itoa(int(prio))))
		_, _ = h.Write([]byte(";"))
	}

	return fmt.Sprintf("%x", h.Sum64())
}

// IsPreemptor returns true if the preemptorUID matches the other podUID.
func IsPreemptor(PodUID types.UID, preemptorUID types.UID) bool {
	return string(PodUID) != "" && string(preemptorUID) != "" && preemptorUID == PodUID
}

// WorkloadKind represents the type of workload.
func (wk WorkloadKey) String() string {
	switch wk.Kind {
	case wkReplicaSet:
		return "rs:" + combineNsName(wk.Namespace, wk.Name)
	case wkStatefulSet:
		return "ss:" + combineNsName(wk.Namespace, wk.Name)
	case wkDaemonSet:
		return "ds:" + combineNsName(wk.Namespace, wk.Name)
	case wkJob:
		return "job:" + combineNsName(wk.Namespace, wk.Name)
	default:
		return combineNsName(wk.Namespace, wk.Name)
	}
}

// topWorkload returns the top-level workload controller of a pod, if any.
func topWorkload(p *v1.Pod) (WorkloadKey, bool) {
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

// WorkloadKind represents the kind of workload.
type WorkloadKind int

const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

// WorkloadKey is a key to identify a workload.
type WorkloadKey struct {
	// What kind of workload
	Kind WorkloadKind
	// Namespace of the workload
	Namespace string
	// Name of the workload
	Name string
}

// PendingSnapshot bundles the pieces of state that both the periodic and
// free-time loops need in order to decide whether to run the solver.
type PendingSnapshot struct {
	PendingUIDs  map[types.UID]struct{}
	PendingCount int
	Fingerprint  string     // clusterFingerprint(nodes, pods)
	Pods         []*v1.Pod  // live snapshot (for priority checks)
	Nodes        []*v1.Node // live snapshot (for solver input)
}

// buildPendingSnapshot:
//   - lists current pods and nodes via informers
//   - builds the set of Pending pod UIDs
//   - computes the baseline cluster fingerprint (usable nodes + running pods).
func (pl *SharedState) buildPendingSnapshot() (*PendingSnapshot, error) {
	pods, err := pl.getPods()
	if err != nil {
		return nil, fmt.Errorf("buildPendingSnapshot: getPods failed: %w", err)
	}
	nodes, err := pl.getNodes()
	if err != nil {
		return nil, fmt.Errorf("buildPendingSnapshot: getNodes failed: %w", err)
	}

	pendingSet := make(map[types.UID]struct{})
	for _, p := range pods {
		if p == nil || p.DeletionTimestamp != nil {
			continue
		}
		// You currently check Status.Phase == "Pending" – use constant for clarity.
		if p.Status.Phase != v1.PodPending {
			continue
		}
		pendingSet[p.UID] = struct{}{}
	}

	fp := clusterFingerprint(nodes, pods)

	return &PendingSnapshot{
		PendingUIDs:  pendingSet,
		PendingCount: len(pendingSet),
		Fingerprint:  fp,
		Pods:         pods,
		Nodes:        nodes,
	}, nil
}
