// helpers.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// ----- Helpers for strategies -------
// Optimizer cadence (per-preemptor vs. batches)
func optimizeForEvery() bool  { return OptimizeCadence == OptimizeForEvery }
func optimizeInBatches() bool { return OptimizeCadence == OptimizeInBatches }

// Action point (PreEnqueue vs. PostFilter)
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

func modeToString() string {
	a := "ForEvery"
	if optimizeInBatches() {
		a = "InBatches"
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// ---------- Helpers for objects --------------

func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func nsOf(nsSlashName string) string {
	if i := strings.IndexByte(nsSlashName, '/'); i >= 0 {
		return nsSlashName[:i]
	}
	return "default"
}

func splitNamespaceName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
}

func (pl *MyCrossNodePreemption) getNodes() ([]*v1.Node, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.Handle.SharedInformerFactory().Core().V1().Nodes().Lister().List(labels.Everything())
}

func (pl *MyCrossNodePreemption) getPods() ([]*v1.Pod, error) {
	// Do not use SnapshotLister as it may return stale data
	return pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister().List(labels.Everything())
}

func isNodeUsable(n *v1.Node) bool {
	if n == nil {
		return false
	}
	isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"
	return !isCP &&
		!n.Spec.Unschedulable &&
		n.Status.Allocatable.Cpu().MilliValue() > 0 &&
		n.Status.Allocatable.Memory().Value() > 0
}

// --------- Pod specifications functions ---------

func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Cpu().MilliValue()
	}
	return total
}

func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
	}
	return total
}

func getPodPriority(p *v1.Pod) int32 {
	if p.Spec.Priority != nil {
		return *p.Spec.Priority
	}
	return 0
}

// --------- Pod set functions ---------

func newPodSet() *podSet { return &podSet{m: make(map[types.UID]podKey)} }

func (s *podSet) AddRef(uid types.UID, ns, name string) {
	s.mu.Lock()
	s.m[uid] = podKey{UID: uid, Namespace: ns, Name: name}
	s.mu.Unlock()
}

func (s *podSet) AddPod(p *v1.Pod) {
	if p == nil {
		return
	}
	s.AddRef(p.UID, p.Namespace, p.Name)
}

func (s *podSet) Remove(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

func (s *podSet) Clear() {
	s.mu.Lock()
	s.m = make(map[types.UID]podKey)
	s.mu.Unlock()
}

func (s *podSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

func (s *podSet) Snapshot() map[types.UID]podKey {
	s.mu.RLock()
	out := make(map[types.UID]podKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}

func (pl *MyCrossNodePreemption) pruneSetStale(set *podSet, keep func(cur *v1.Pod) bool) int {
	if set == nil || set.Size() == 0 {
		return 0
	}

	snap := set.Snapshot()
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()

	removed := 0
	for uid, key := range snap {
		cur, err := podLister.Pods(key.Namespace).Get(key.Name)
		switch {
		case apierrors.IsNotFound(err):
			set.Remove(uid)
			removed++
		case err != nil:
			// Conservative; keep if lister errored
		default:
			// Drop if recreated/terminating
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil {
				set.Remove(uid)
				removed++
				continue
			}
			// Apply caller-specific predicate
			if keep != nil && !keep(cur) {
				set.Remove(uid)
				removed++
			}
		}
	}
	return removed
}

func (pl *MyCrossNodePreemption) activatePodsFromSet(set *podSet) bool {
	if set == nil || set.Size() == 0 {
		return false
	}
	snap := set.Snapshot()
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	toAct := make(map[string]*v1.Pod, len(snap))
	for _, k := range snap {
		if p, err := l.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			toAct[k.Namespace+"/"+k.Name] = p
		}
	}
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		return true
	}
	return false
}

func (pl *MyCrossNodePreemption) activateBlockedPods() {
	if pl.activatePodsFromSet(pl.Blocked) {
		pl.Blocked.Clear()
	}
}

func (pl *MyCrossNodePreemption) activateBatchedPods(podsToRemove []*v1.Pod) {
	if pl.activatePodsFromSet(pl.Batched) {
		for _, p := range podsToRemove {
			pl.Batched.Remove(p.UID)
		}
	}
}

func (pl *MyCrossNodePreemption) pruneStaleSetEntries(set *podSet) int {
	rem := pl.pruneSetStale(set, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep function: keep only pending pods
	})
	if rem > 0 {
		klog.V(2).InfoS("Pruned stale entries", "removed", rem)
	}
	return rem
}

// ---------- Helpers for workloads --------------

func (wk WorkloadKey) String() string {
	switch wk.Kind {
	case wkReplicaSet:
		return "rs:" + wk.Namespace + "/" + wk.Name
	case wkStatefulSet:
		return "ss:" + wk.Namespace + "/" + wk.Name
	case wkDaemonSet:
		return "ds:" + wk.Namespace + "/" + wk.Name
	case wkJob:
		return "job:" + wk.Namespace + "/" + wk.Name
	default:
		return wk.Namespace + "/" + wk.Name
	}
}

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

func workloadEqual(a, b WorkloadKey) bool {
	return a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

func workloadSelector(ctx context.Context, cli kubernetes.Interface, wk WorkloadKey) (metav1.LabelSelector, error) {
	switch wk.Kind {
	case wkReplicaSet:
		rs, err := cli.AppsV1().ReplicaSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *rs.Spec.Selector, nil
	case wkStatefulSet:
		ss, err := cli.AppsV1().StatefulSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ss.Spec.Selector, nil
	case wkDaemonSet:
		ds, err := cli.AppsV1().DaemonSets(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		return *ds.Spec.Selector, nil
	case wkJob:
		job, err := cli.BatchV1().Jobs(wk.Namespace).Get(ctx, wk.Name, metav1.GetOptions{})
		if err != nil {
			return metav1.LabelSelector{}, err
		}
		if job.Spec.Selector != nil {
			return *job.Spec.Selector, nil
		}
		return metav1.LabelSelector{MatchLabels: job.Spec.Template.Labels}, nil
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unsupported workload kind for selector: %v", wk.Kind)
	}
}

func workloadParseKey(s string) (WorkloadKey, bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return WorkloadKey{}, false
	}
	kindStr, rest := s[:colon], s[colon+1:]
	ns, name := splitNamespaceName(rest)

	var k WorkloadKind
	switch kindStr {
	case "rs":
		k = wkReplicaSet
	case "ss":
		k = wkStatefulSet
	case "ds":
		k = wkDaemonSet
	case "job":
		k = wkJob
	default:
		return WorkloadKey{}, false
	}
	return WorkloadKey{Kind: k, Namespace: ns, Name: name}, true
}

// -------------- Other utility functions --------------

func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
}

func (pl *MyCrossNodePreemption) recreatePod(ctx context.Context, orig *v1.Pod, _ string) error {
	newp := orig.DeepCopy()
	newp.GenerateName = ""
	newp.ResourceVersion = ""
	newp.UID = ""
	newp.Status = v1.PodStatus{}
	newp.Spec.NodeName = "" // no direct binding
	newp.Spec.NodeSelector = map[string]string{}

	if _, err := pl.Client.CoreV1().Pods(orig.Namespace).Create(ctx, newp, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create pod %s: %w", podRef(newp), err)
	}
	return nil
}

func (pl *MyCrossNodePreemption) evictPod(ctx context.Context, pod *v1.Pod) error {
	grace := int64(0)
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

func (pl *MyCrossNodePreemption) waitPodsGone(ctx context.Context, pods []*v1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	type key struct{ ns, name, uid string }
	remaining := make(map[key]struct{}, len(pods))
	for _, p := range pods {
		remaining[key{ns: p.Namespace, name: p.Name, uid: string(p.UID)}] = struct{}{}
	}

	// Use context's deadline if any, else default to 2m
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if len(remaining) == 0 {
			return true, nil
		}
		for k := range remaining {
			got, err := pl.Client.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				delete(remaining, k)
				continue
			}
			if err != nil {
				// transient error: keep polling
				return false, nil
			}
			if string(got.UID) != k.uid {
				// name reused; original is gone
				delete(remaining, k)
			}
		}
		if len(remaining) == 0 {
			return true, nil
		}
		klog.V(2).InfoS("Waiting for targeted pods to disappear", "remaining", len(remaining))
		return false, nil
	})
}

func (pl *MyCrossNodePreemption) tryEnterActive() bool {
	return pl.Active.CompareAndSwap(false, true)
}

func (pl *MyCrossNodePreemption) leaveActive() {
	pl.Active.Store(false)
}
