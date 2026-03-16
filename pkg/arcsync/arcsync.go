package arcsync

import (
	"context"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/klog/v2"
)

const (
	Name = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
)

type reservation struct {
	nodeName  string
	count     int64
	timestamp time.Time
	baseName  string
}

type ARCSync struct {
	handle framework.Handle
	// key: podUID, value: reservation
	inFlightReservations map[string]reservation
	mu                   sync.Mutex
}

var _ framework.PreFilterPlugin = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{
		handle:               h,
		inFlightReservations: make(map[string]reservation),
	}, nil
}

func (pl *ARCSync) Name() string {
	return Name
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

	pl.mu.Lock()
	defer pl.mu.Unlock()

	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	// 1. 第一步：统计物理占用并识别活跃的 Workflow
	nodePhysicalUsage := make(map[string]int64)
	activeWorkflows := make(map[string]bool)
	knownPodUIDs := make(map[string]bool)

	for _, nodeInfo := range nodeInfos {
		nodeName := nodeInfo.Node().Name
		var physUsage int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed || p.UID == pod.UID {
				continue
			}
			knownPodUIDs[string(p.UID)] = true

			// 获取该 Pod 的基准任务名 (用于识别 Workflow)
			baseName := getBaseName(p.Name)
			if p.Name != baseName { // 名字不同，说明带了 -workflow 后缀
				activeWorkflows[baseName] = true
			}

			// 计算物理占用: max(npu-count 标签, Request)
			var podUsage int64 = 0
			// A. 从 Resources.Requests 统计
			for _, container := range p.Spec.Containers {
				if q, exists := container.Resources.Requests[fullResourceName]; exists {
					podUsage += q.Value()
				}
			}
			// B. 从 npu-count 标签统计 (此处不看 required-npu-count 标签)
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

	// 2. 第二步：累加逻辑占用 (带状态位去重)
	nodeTotalOccupied := make(map[string]int64)
	for nodeName, usage := range nodePhysicalUsage {
		nodeTotalOccupied[nodeName] = usage
	}

	now := time.Now()
	for uid, res := range pl.inFlightReservations {
		// 自愈逻辑
		if !knownPodUIDs[uid] && now.Sub(res.timestamp) > 2*time.Minute {
			delete(pl.inFlightReservations, uid)
			continue
		}

		// 核心逻辑：如果该任务已经拉起了 Workflow Pod (物理层面已统计)，则不再叠加逻辑预留
		if activeWorkflows[res.baseName] {
			klog.V(4).InfoS("ARCSync: Skipping logical reservation, workflow already active",
				"job", res.baseName, "node", res.nodeName)
			continue
		}

		nodeTotalOccupied[res.nodeName] += res.count
	}

	// 3. 寻找满足需求的节点
	bestNode := ""
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		allocatable := node.Status.Allocatable[fullResourceName]
		occupied := nodeTotalOccupied[node.Name]

		freeNPU := allocatable.Value() - occupied

		if freeNPU >= int64(reqCount) {
			bestNode = node.Name
			klog.V(4).InfoS("ARCSync: Potential node found", "node", bestNode, "free", freeNPU)
			break
		}
	}

	if bestNode == "" {
		klog.InfoS("ARCSync: PreFilter rejected pod (insufficient resources)",
			"pod", pod.Name, "required", reqCount)
		return nil, framework.NewStatus(framework.Unschedulable, "No node has enough available NPU slots (physical + logical)")
	}

	// 4. 记录预留：逻辑绑定到选中的节点
	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}
	pl.inFlightReservations[podKey] = reservation{
		nodeName:  bestNode,
		count:     int64(reqCount),
		timestamp: now,
		baseName:  getBaseName(pod.Name),
	}

	klog.InfoS("ARCSync: PreFilter passed, logical slot secured",
		"pod", pod.Name, "targetNode", bestNode)
	return nil, framework.NewStatus(framework.Success, "")
}


func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
