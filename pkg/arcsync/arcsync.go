package arcsync

import (
	"context"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name              = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
	stateKey          = Name + "/state"
)

type reservation struct {
	nodeName  string
	count     int64
	timestamp time.Time
	baseName  string
}

type ARCSync struct {
	handle               framework.Handle
	inFlightReservations map[string]reservation
	mu                   sync.Mutex
}

type preFilterState struct {
	requiredCount int64
	resourceName  v1.ResourceName
	nodeFreeNPU   map[string]int64
}

func (s *preFilterState) Clone() framework.StateData {
	return s
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.FilterPlugin = &ARCSync{}
var _ framework.ScorePlugin = &ARCSync{}
var _ framework.ReservePlugin = &ARCSync{}
var _ framework.PostBindPlugin = &ARCSync{}
var _ framework.EnqueueExtensions = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{
		handle:               h,
		inFlightReservations: make(map[string]reservation),
	}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

func (pl *ARCSync) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete | framework.Update}},
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add | framework.Update}},
	}, nil
}

func canScheduleOnNode(pod *v1.Pod, node *v1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != v1.TaintEffectNoSchedule && taint.Effect != v1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, toleration := range pod.Spec.Tolerations {
			if toleration.ToleratesTaint(&taint) {
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

func getBaseName(name string) string {
	suffix := "-workflow"
	if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}

func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	pl.mu.Lock()

	activeWorkflows := make(map[string]bool)
	knownPodUIDs := make(map[string]bool)
	nodePhysicalUsage := make(map[string]int64)

	for _, nodeInfo := range nodeInfos {
		nodeName := nodeInfo.Node().Name
		var physUsage int64
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed || p.UID == pod.UID {
				continue
			}
			knownPodUIDs[string(p.UID)] = true
			baseName := getBaseName(p.Name)
			if p.Name != baseName {
				activeWorkflows[baseName] = true
			}
			var podUsage int64
			for _, container := range p.Spec.Containers {
				if q, exists := container.Resources.Requests[fullResourceName]; exists {
					podUsage += q.Value()
				}
			}
			if val, exists := p.Labels[AllocatedNPUCount]; exists {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(val, 10, 64)
					if count > podUsage {
						podUsage = count
					}
				}
			}
			physUsage += podUsage
		}
		nodePhysicalUsage[nodeName] = physUsage
	}

	nodeTotalOccupied := make(map[string]int64)
	for nodeName, usage := range nodePhysicalUsage {
		nodeTotalOccupied[nodeName] = usage
	}

	now := time.Now()
	for uid, res := range pl.inFlightReservations {
		if !knownPodUIDs[uid] && now.Sub(res.timestamp) > 2*time.Minute {
			delete(pl.inFlightReservations, uid)
			continue
		}
		if activeWorkflows[res.baseName] {
			continue
		}
		nodeTotalOccupied[res.nodeName] += res.count
	}

	pl.mu.Unlock()

	nodeFreeNPU := make(map[string]int64)
	hasCandidate := false
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(pod, node) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		free := allocatable.Value() - nodeTotalOccupied[node.Name]
		nodeFreeNPU[node.Name] = free
		if free >= int64(reqCount) {
			hasCandidate = true
		}
	}

	if !hasCandidate {
		klog.InfoS("ARCSync: PreFilter rejected pod (no node has enough NPU)",
			"pod", pod.Name, "required", reqCount)
		return nil, framework.NewStatus(framework.Unschedulable, "No node has enough available NPU slots")
	}

	state.Write(stateKey, &preFilterState{
		requiredCount: int64(reqCount),
		resourceName:  fullResourceName,
		nodeFreeNPU:   nodeFreeNPU,
	})
	return nil, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func (pl *ARCSync) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	nodeName := nodeInfo.Node().Name
	free, exists := data.nodeFreeNPU[nodeName]
	if !exists {
		return framework.NewStatus(framework.Unschedulable, "node not eligible for NPU scheduling")
	}
	if free < data.requiredCount {
		return framework.NewStatus(framework.Unschedulable, "insufficient NPU on node")
	}
	return framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	s, err := state.Read(stateKey)
	if err != nil {
		return 0, framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)
	free := data.nodeFreeNPU[nodeName]
	score := free
	if score > framework.MaxNodeScore {
		score = framework.MaxNodeScore
	}
	if score < 0 {
		score = 0
	}
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (pl *ARCSync) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}

	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.inFlightReservations[podKey] = reservation{
		nodeName:  nodeName,
		count:     data.requiredCount,
		timestamp: time.Now(),
		baseName:  getBaseName(pod.Name),
	}
	klog.InfoS("ARCSync: Reserved NPU slots", "pod", pod.Name, "node", nodeName, "count", data.requiredCount)
	return framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.inFlightReservations, podKey)
	klog.InfoS("ARCSync: Unreserved NPU slots", "pod", pod.Name, "node", nodeName)
}

func (pl *ARCSync) PostBind(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.inFlightReservations, podKey)
	klog.InfoS("ARCSync: PostBind cleared reservation", "pod", pod.Name, "node", nodeName)
}
