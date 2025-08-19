// common.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// ---------------------------- Plugin wiring ----------------------------

const (
	Name                   = "MyCrossNodePreemption"
	Version                = "v1.0.4"
	DeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
)

type MyCrossNodePreemption struct {
	handle framework.Handle
	client kubernetes.Interface
}

func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	if obj != nil {
		klog.V(2).InfoS("Plugin configuration", "config", obj)
	}
	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}
	klog.InfoS("Plugin initialized", "name", Name, "version", Version)
	return &MyCrossNodePreemption{handle: h, client: client}, nil
}

// ---------------------------- Plan schema (shared) ----------------------------

type StoredPlan struct {
	SchemaVersion    string                    `json:"schemaVersion"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	Plugin           string                    `json:"plugin"`
	Version          string                    `json:"pluginVersion"`
	PendingPod       string                    `json:"pendingPod"` // ns/name
	PendingUID       string                    `json:"pendingUID"`
	TargetNode       string                    `json:"targetNode"`
	StopTheWorld     bool                      `json:"stopTheWorld"`
	Completed        bool                      `json:"completed"`
	SolverOutput     *solverOutput             `json:"solverOutput,omitempty"`
	Plan             PodAssignmentPlanLite     `json:"plan"`
	PlacementsByName map[string]string         `json:"placementsByName,omitempty"` // standalone pods -> node
	RSDesiredPerNode map[string]map[string]int `json:"rsDesiredPerNode,omitempty"` // "<ns>/<rs>" -> node -> count
	Progress         *PlanProgress             `json:"progress,omitempty"`
}

type PodAssignmentPlanLite struct {
	TargetNode string         `json:"targetNode"`
	Movements  []MovementLite `json:"movements"`
	Evictions  []PodRefLite   `json:"evictions"`
}
type MovementLite struct {
	Pod      PodRefLite `json:"pod"`
	FromNode string     `json:"fromNode"`
	ToNode   string     `json:"toNode"`
	CPUm     int64      `json:"cpu_m"`
	MemBytes int64      `json:"mem_bytes"`
}
type PodRefLite struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type PodAssignmentPlan struct {
	TargetNode     string
	PodMovements   []PodMovement
	VictimsToEvict []*v1.Pod
}
type PodMovement struct {
	Pod           *v1.Pod
	FromNode      string
	ToNode        string
	CPURequest    int64 // milliCPU
	MemoryRequest int64 // bytes
}

type PlanProgress struct {
	PendingBound bool                      `json:"pendingBound"`
	StandaloneOK map[string]bool           `json:"standaloneOK"` // key: "ns/name" (or just name if you prefer); true when satisfied
	RSRemaining  map[string]map[string]int `json:"rsRemaining"`  // "<ns>/<rs>" -> node -> remaining binds to observe
}

// ---------------------------- ConfigMap labels / constants (shared) -----------

const (
	exportNamespace         = "kube-system"
	exportCMLabelKey        = "scheduler.x/crossnode-plan"
	exportCMLabelVal        = "true"
	exportCMLabelActiveKey  = "scheduler.x/crossnode-plan-active"
	exportCMLabelActiveTrue = "true"
)

// ---------------------------- Solver I/O (shared types) ----------------------

type solverNode struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"` // milliCPU
	RAM    int64             `json:"ram"` // bytes
	Labels map[string]string `json:"labels,omitempty"`
}
type solverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	RAM       int64  `json:"ram"`
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`
	Protected bool   `json:"protected,omitempty"`
}
type solverInput struct {
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Preemptor      solverPod    `json:"preemptor"`
	Nodes          []solverNode `json:"nodes"`
	Pods           []solverPod  `json:"pods"`
}
type solverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type solverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode"`
	Placements    map[string]string `json:"placements"` // uid -> node
	Movements     map[string]string `json:"movements"`  // optional
	Evictions     []solverEviction  `json:"evictions"`
}

// ---------------------------- Plan CM helpers (shared) -----------------------

func (pl *MyCrossNodePreemption) deactivateOldPlans(ctx context.Context) error {
	list, err := pl.client.CoreV1().ConfigMaps(exportNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", exportCMLabelActiveKey, exportCMLabelActiveTrue),
	})
	if err != nil {
		return err
	}
	for i := range list.Items {
		_ = pl.client.CoreV1().ConfigMaps(exportNamespace).Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{})
	}
	return nil
}

func (pl *MyCrossNodePreemption) loadActivePlan(ctx context.Context) (*StoredPlan, string, error) {
	list, err := pl.client.CoreV1().ConfigMaps(exportNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", exportCMLabelActiveKey, exportCMLabelActiveTrue),
	})
	if err != nil {
		return nil, "", err
	}
	if len(list.Items) == 0 {
		return nil, "", nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.Time.After(list.Items[j].CreationTimestamp.Time)
	})
	cm := list.Items[0]
	raw := cm.Data["plan.json"]
	if raw == "" {
		return nil, "", fmt.Errorf("active plan missing plan.json")
	}
	var sp StoredPlan
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		return nil, "", err
	}
	return &sp, cm.Name, nil
}

func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	// TODO: Update the config map "Completed" flag and mark config map inactive. Do not delete it.
	if err := pl.client.CoreV1().ConfigMaps(exportNamespace).Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil {
		// If the ConfigMap is not found, it may have been deleted already
		if apierrors.IsNotFound(err) {
			klog.V(2).InfoS("ConfigMap not found; it may have been deleted already", "cmName", cmName)
		} else {
			klog.ErrorS(err, "Failed to delete completed plan ConfigMap", "cmName", cmName)
		}
	}
}

// ---------------------------- Completion check (shared) ----------------------

// isPlanCompleted checks the *live cluster* (client-go) instead of the snapshot.
// It also ensures the pending preemptor is bound to TargetNode.
func (pl *MyCrossNodePreemption) isPlanCompleted(ctx context.Context, sp *StoredPlan) (bool, error) {
	// A) Preemptor must be bound to TargetNode
	pns, pname := splitNSName(sp.PendingPod)
	preemptor, err := pl.client.CoreV1().Pods(pns).Get(ctx, pname, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get pending pod: %w", err)
	}
	if preemptor.Spec.NodeName != sp.TargetNode {
		return false, nil
	}

	// B) Standalone/name-addressed pods placed on their target node
	for name, node := range sp.PlacementsByName {
		ns := nsOf(sp.PendingPod)
		if strings.Contains(name, "/") {
			ns, name = splitNSName(name)
		}
		pod, err := pl.client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
		}
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName != node {
			return false, nil
		}
	}

	// C) RS per-node quotas satisfied
	for rsKeyStr, perNode := range sp.RSDesiredPerNode {
		ns, rsName := splitNSName(rsKeyStr)

		rs, err := pl.client.AppsV1().ReplicaSets(ns).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get rs %s/%s: %w", ns, rsName, err)
		}
		sel, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return false, fmt.Errorf("selector for rs %s/%s: %w", ns, rsName, err)
		}
		podList, err := pl.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return false, fmt.Errorf("list rs pods %s/%s: %w", ns, rsName, err)
		}

		counts := map[string]int{}
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.DeletionTimestamp != nil {
				continue // ignore terminating
			}
			if r, ok := owningReplicaSet(pod); !ok || r != rsName {
				continue // make sure it's this RS
			}
			if pod.Spec.NodeName == "" {
				continue // not yet scheduled; don't count
			}
			counts[pod.Spec.NodeName]++
		}

		for node, want := range perNode {
			if counts[node] < want {
				return false, nil
			}
		}
	}

	return true, nil
}

// ---------------------------- Utilities (shared) -----------------------------

func rsKey(ns, rs string) string { return ns + "/" + rs }

func nsOf(nsSlashName string) string {
	if i := strings.IndexByte(nsSlashName, '/'); i >= 0 {
		return nsSlashName[:i]
	}
	return "default"
}

func splitNSName(s string) (string, string) {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return "default", s
	}
	return s[:i], s[i+1:]
}

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

func getPodCPURequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceCPU]; !req.IsZero() {
			total += req.MilliValue()
		}
	}
	return total
}

func getPodMemoryRequest(p *v1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		if req := c.Resources.Requests[v1.ResourceMemory]; !req.IsZero() {
			total += req.Value()
		}
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

func toleratesNoScheduleTaints(pod *v1.Pod, taints []v1.Taint) bool {
	for _, t := range taints {
		if t.Effect != v1.TaintEffectNoSchedule {
			continue
		}
		tolerated := false
		for _, tol := range pod.Spec.Tolerations {
			if tol.ToleratesTaint(&t) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func isControlPlane(n *v1.Node) bool {
	if n == nil {
		return false
	}
	labels := n.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	if n.Name == "control-plane" || n.Name == "kind-control-plane" {
		return true
	}
	return false
}

func isNodeUsableFor(pod *v1.Pod, ni *framework.NodeInfo) bool {
	n := ni.Node()
	if n == nil {
		return false
	}
	if isControlPlane(n) {
		return false
	}
	if n.Spec.Unschedulable {
		return false
	}
	if !toleratesNoScheduleTaints(pod, n.Spec.Taints) {
		return false
	}
	if ni.Allocatable.MilliCPU <= 0 || ni.Allocatable.Memory <= 0 {
		return false
	}
	return true
}

func nsNameKey(ns, name string) string { return ns + "/" + name }
