// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	// ======= Batch strategy =======
	// Batch strategy: BatchPostFilter, BatchPreEnqueue, BatchOff
	BatchMode BatchIngressMode = BatchPreEnqueue

	// Every-preemptor strategy (mutually exclusive with any BatchMode != BatchOff)
	ModeEveryPreemptor = false
)

// ---------------------------- Plugin wiring -----------------------
func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// Exactly one strategy
	if (batchingEnabled() && ModeEveryPreemptor) || (!batchingEnabled() && !ModeEveryPreemptor) {
		return nil, fmt.Errorf("%s: invalid config: enable exactly one of {BatchMode!=BatchOff, PostFilterSinglePreemptor}", Name)
	}

	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}

	pl := &MyCrossNodePreemption{
		Handle:  h,
		Client:  client,
		Blocked: newPodSet(),
		Batched: newPodSet(),
	}
	klog.InfoS("Plugin initialized", "name", Name, "version", Version,
		"batchMode", batchModeToString(), "everyPreemptorMode", ModeEveryPreemptor)

	if batchingEnabled() {
		go pl.batchLoop(context.Background())
	}
	return pl, nil
}

// ---------------------------- Types -----------------------

type MyCrossNodePreemption struct {
	Handle       framework.Handle
	Client       kubernetes.Interface
	ActivePlan   atomic.Value              // stores *StoredPlan or nil
	ActivePlanID atomic.Value              // string (e.g., cmName or a UUID)
	SlotsPtr     atomic.Pointer[PlanSlots] // atomic planSlots pointer
	Blocked      *podSet
	Batched      *podSet
}

type WorkloadKind int

type WorkloadKey struct {
	Kind      WorkloadKind
	Namespace string
	Name      string
}

type BlockedPodInfo struct {
	Namespace string
	Name      string
}

// Deployment -> ReplicaSet
// CronJob -> Job
const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

// has at least one usable node in the scheduler snapshot
func (pl *MyCrossNodePreemption) haveUsableNodes() bool {
	nodes, err := pl.getNodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if isNodeUsable(n) {
			return true
		}
	}
	return false
}

func nsOf(nsSlashName string) string {
	if i := strings.IndexByte(nsSlashName, '/'); i >= 0 {
		return nsSlashName[:i]
	}
	return "default"
}

func splitNSName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
}

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

func podRef(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

type podKey struct {
	UID       types.UID
	Namespace string
	Name      string
}

type podSet struct {
	mu sync.RWMutex
	m  map[types.UID]podKey
}

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

// Snapshot returns a copy of keys to avoid holding locks while calling the lister.
func (s *podSet) Snapshot() map[types.UID]podKey {
	s.mu.RLock()
	out := make(map[types.UID]podKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}

// pruneSetStale removes entries whose live pod either doesn't exist, changed UID,
// is terminating, or fails the optional keep predicate.
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
			// conservative: keep if lister errored
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

func selectorForWorkload(ctx context.Context, cli kubernetes.Interface, wk WorkloadKey) (metav1.LabelSelector, error) {
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

func parseWorkloadKey(s string) (WorkloadKey, bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return WorkloadKey{}, false
	}
	kindStr, rest := s[:colon], s[colon+1:]
	ns, name := splitNSName(rest)

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

func (pl *MyCrossNodePreemption) activateBlockedPods() {
	if pl.Blocked == nil {
		return
	}
	snap := pl.Blocked.Snapshot()
	if len(snap) == 0 {
		return
	}
	l := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	pods := make(map[string]*v1.Pod, len(snap))
	for _, k := range snap {
		p, err := l.Pods(k.Namespace).Get(k.Name)
		if err != nil {
			klog.ErrorS(err, "lister failed", "pod", k.Namespace+"/"+k.Name)
			continue
		}
		pods[k.Namespace+"/"+k.Name] = p
	}
	if len(pods) > 0 {
		pl.Handle.Activate(klog.Background(), pods)
	}
	pl.Blocked.Clear()
}

func bytesToMiB(b int64) int64 {
	return b / (1024 * 1024)
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

func ptr[T any](v T) *T { return &v }

// Blocked: drop if the pod is already scheduled, terminating, recreated, or gone.
func (pl *MyCrossNodePreemption) pruneBlockedStale() int {
	rem := pl.pruneSetStale(pl.Blocked, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep only truly unscheduled
	})
	if rem > 0 {
		klog.V(2).InfoS("Pruned stale entries from blocked set", "removed", rem)
	}
	return rem
}

func (pl *MyCrossNodePreemption) numOfUnscheduledPods() int {
	nodeInfos, err := pl.Handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return -1
	}
	var unscheduledPods []*v1.Pod
	for _, ni := range nodeInfos {
		for _, pi := range ni.Pods {
			if pi.Pod.Status.Phase == v1.PodPending {
				unscheduledPods = append(unscheduledPods, pi.Pod)
			}
		}
	}
	return len(unscheduledPods)
}
