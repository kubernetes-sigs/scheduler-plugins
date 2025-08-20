// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name                   = "MyCrossNodePreemption"
	Version                = "v1.0.4"
	DeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

	ConfigMapNamespace  = "kube-system"
	ConfigMapLabelKey   = "crossnode-plan"
	ConfigMapNamePrefix = "crossnode-plan-" // used to find plan CMs

	PollTimeout  = 10 * time.Second
	PollInterval = 250 * time.Millisecond

	PythonSolverPath    = "/opt/solver/main.py"
	PythonSolverTimeout = 60 * time.Second

	DeletionCostTarget = math.MinInt32
	DeletionCostKeep   = math.MaxInt32
)

type MyCrossNodePreemption struct {
	handle framework.Handle
	client kubernetes.Interface
}

type StoredPlan struct {
	Completed        bool                      `json:"completed"`
	CompletedAt      *time.Time                `json:"completedAt,omitempty"`
	GeneratedAt      time.Time                 `json:"generatedAt"`
	PluginVersion    string                    `json:"pluginVersion"`
	PendingPod       string                    `json:"pendingPod"` // ns/name
	PendingUID       string                    `json:"pendingUID"`
	TargetNode       string                    `json:"targetNode"`
	SolverOutput     *SolverOutput             `json:"solverOutput,omitempty"`
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

type SolverNode struct {
	Name   string            `json:"name"`
	CPU    int64             `json:"cpu"` // milliCPU
	RAM    int64             `json:"ram"` // bytes
	Labels map[string]string `json:"labels,omitempty"`
}
type SolverPod struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	RAM       int64  `json:"ram"`
	Priority  int32  `json:"priority"`
	Where     string `json:"where"`
	Protected bool   `json:"protected,omitempty"`
}
type SolverInput struct {
	TimeoutMs      int64        `json:"timeout_ms"`
	IgnoreAffinity bool         `json:"ignore_affinity"`
	Preemptor      SolverPod    `json:"preemptor"`
	Nodes          []SolverNode `json:"nodes"`
	Pods           []SolverPod  `json:"pods"`
}
type SolverEviction struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type SolverOutput struct {
	Status        string            `json:"status"`
	NominatedNode string            `json:"nominatedNode"`
	Placements    map[string]string `json:"placements"` // uid -> node
	Movements     map[string]string `json:"movements"`  // optional
	Evictions     []SolverEviction  `json:"evictions"`
}

// ---------------------------- Plugin wiring -----------------------
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

// ---------------------------- Plan helpers (shared) -----------------------

// listPlans returns newest-first plan ConfigMaps found by label.
func (pl *MyCrossNodePreemption) listPlans(ctx context.Context) ([]v1.ConfigMap, error) {
	lst, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", ConfigMapLabelKey, "true"),
		},
	)
	if err != nil {
		return nil, err
	}
	// Sort newest first
	sort.Slice(lst.Items, func(i, j int) bool {
		return lst.Items[i].CreationTimestamp.Time.After(lst.Items[j].CreationTimestamp.Time)
	})
	return lst.Items, nil
}

// loadActivePlan loads the active plan from the list of plans
func (pl *MyCrossNodePreemption) loadActivePlan(ctx context.Context) (*StoredPlan, string, error) {
	items, err := pl.listPlans(ctx)
	if err != nil {
		return nil, "", err
	}
	for i := range items {
		raw := items[i].Data["plan.json"]
		if raw == "" {
			continue
		}
		var sp StoredPlan
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			continue
		}
		if !sp.Completed {
			return &sp, items[i].Name, nil
		}
	}
	return nil, "", nil
}

// markPlanCompleted sets Completed=true in json (i.e. not active plan).
// Keeps the CM but prunes old history.
func (pl *MyCrossNodePreemption) markPlanCompleted(ctx context.Context, cmName string) {
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || cm == nil {
			return nil // already gone
		}
		if err != nil {
			return err
		}
		raw := cm.Data["plan.json"]
		if raw == "" {
			return nil
		}
		var sp StoredPlan
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			klog.ErrorS(err, "markPlanCompleted: cannot decode plan.json", "configMap", cmName)
			return nil
		}
		if !sp.Completed {
			now := time.Now().UTC()
			sp.Completed = true
			sp.CompletedAt = &now
			b, _ := json.MarshalIndent(&sp, "", "  ")
			patch := []byte(fmt.Sprintf(`{"data":{"plan.json":%q}}`, string(b)))
			_, err = pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).
				Patch(ctx, cmName, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		}
		return nil
	})

	// Wait until Completed flag is set.
	_ = wait.PollUntilContextTimeout(ctx, PollInterval, PollTimeout, true,
		func(ctx context.Context) (bool, error) {
			cm, err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, cmName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || cm == nil {
				return true, nil
			}
			if err != nil {
				return false, nil
			}
			var sp StoredPlan
			if err := json.Unmarshal([]byte(cm.Data["plan.json"]), &sp); err != nil {
				return false, nil
			}
			return sp.Completed, nil
		})

	// Prune history (JSON-only semantics)
	if err := pl.pruneOldPlans(ctx, 20); err != nil {
		klog.ErrorS(err, "Failed to prune old plans after completion")
	}
}

// pruneOldPlans prunes old plans, keeping only the most recent 'keep' plans.
func (pl *MyCrossNodePreemption) pruneOldPlans(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	items, err := pl.listPlans(ctx)
	if err != nil {
		return err
	}
	if len(items) <= keep {
		return nil
	}

	// Find newest incomplete
	latestIncomplete := ""
	for i := range items {
		raw := items[i].Data["plan.json"]
		if raw == "" {
			continue
		}
		var sp StoredPlan
		if json.Unmarshal([]byte(raw), &sp) == nil && !sp.Completed {
			latestIncomplete = items[i].Name
			break
		}
	}

	// Keep set = newest 'keep', force-include newest incomplete (if outside keep)
	keepSet := make(map[string]struct{}, keep)
	for i := 0; i < len(items) && len(keepSet) < keep; i++ {
		keepSet[items[i].Name] = struct{}{}
	}
	if latestIncomplete != "" {
		if _, ok := keepSet[latestIncomplete]; !ok {
			// evict the oldest among currently-kept to make room
			for i := keep - 1; i >= 0 && i < len(items); i-- {
				if _, ok := keepSet[items[i].Name]; ok && items[i].Name != latestIncomplete {
					delete(keepSet, items[i].Name)
					break
				}
			}
			keepSet[latestIncomplete] = struct{}{}
		}
	}

	// Delete the rest
	for i := range items {
		name := items[i].Name
		if _, ok := keepSet[name]; ok {
			continue
		}
		if err := pl.client.CoreV1().ConfigMaps(ConfigMapNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete old plan ConfigMap", "configMap", name)
		}
	}
	return nil
}

// isPlanCompleted checks if the plan is completed by verifying the state of the cluster.
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
			if r, ok := owningRS(pod); !ok || r != rsName {
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

func splitNSName(s string) (ns, name string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "default", s
}

func owningRS(p *v1.Pod) (string, bool) {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" {
			return o.Name, true
		}
	}
	return "", false
}

func isControlledByRS(p *v1.Pod, rsName string) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller &&
			o.Kind == "ReplicaSet" && o.Name == rsName {
			return true
		}
	}
	return false
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

func isNodeUsable(ni *framework.NodeInfo) bool {
	if ni == nil || ni.Node() == nil {
		return false
	}
	n := ni.Node()
	isCP := n.Labels["node-role.kubernetes.io/control-plane"] != "" ||
		n.Labels["node-role.kubernetes.io/master"] != "" ||
		n.Name == "control-plane" || n.Name == "kind-control-plane"

	return !isCP &&
		!n.Spec.Unschedulable &&
		ni.Allocatable.MilliCPU > 0 &&
		ni.Allocatable.Memory > 0
}

func nsNameKey(ns, name string) string { return ns + "/" + name }
