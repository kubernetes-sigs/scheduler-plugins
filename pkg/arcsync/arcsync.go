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

	// 1. 整理物理占用情况 (按 Job 分组去重)
	// jobUsageOnNode: map[nodeName]map[jobBaseName]usage
	jobUsageOnNode := make(map[string]map[string]int64)
	knownPodUIDs := make(map[string]bool)

	for _, nodeInfo := range nodeInfos {
		nodeName := nodeInfo.Node().Name
		if jobUsageOnNode[nodeName] == nil {
			jobUsageOnNode[nodeName] = make(map[string]int64)
		}

		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed || p.UID == pod.UID {
				continue
			}
			knownPodUIDs[string(p.UID)] = true

			// 获取该 Pod 的基准任务名 (去掉 -workflow 后缀)
			baseName := getBaseName(p.Name)
			var podUsage int64 = 0

			// 逻辑 A: 从 Resources.Requests 统计物理需求 (作为保底)
			for _, container := range p.Spec.Containers {
				if q, exists := container.Resources.Requests[fullResourceName]; exists {
					podUsage += q.Value()
				}
			}

			// 逻辑 B: 从标签统计物理需求 (可能比 Request 更精准，或代表逻辑需求)
			var labelUsage int64 = 0
			if val, exists := p.Labels[AllocatedNPUCount]; exists {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(val, 10, 64)
					labelUsage = count
				}
			}
			if val, exists := p.Labels[RequiredNPUCount]; exists {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(val, 10, 64)
					if count > labelUsage {
						labelUsage = count
					}
				}
			}

			// 取 Request 和 Label 中的较大者
			if labelUsage > podUsage {
				podUsage = labelUsage
			}

			if podUsage > jobUsageOnNode[nodeName][baseName] {
				jobUsageOnNode[nodeName][baseName] = podUsage
			}
		}
	}

	// 2. 整理逻辑预留情况 (继续按 Job 在对应节点上取 max)
	now := time.Now()
	for uid, res := range pl.inFlightReservations {
		// 自愈逻辑
		if !knownPodUIDs[uid] && now.Sub(res.timestamp) > 2*time.Minute {
			delete(pl.inFlightReservations, uid)
			continue
		}

		// 只有当预留量大于该节点上该 Job 已有的物理占用时，才更新（取 max）
		if res.count > jobUsageOnNode[res.nodeName][res.baseName] {
			jobUsageOnNode[res.nodeName][res.baseName] = res.count
		}
	}

	// 3. 计算每个节点的最终可用资源并寻找目标
	bestNode := ""
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		allocatable := node.Status.Allocatable[fullResourceName]

		// 累加该节点上所有 Job 的去重后占用 (物理 + 逻辑)
		totalOccupiedOnNode := int64(0)
		for _, usage := range jobUsageOnNode[node.Name] {
			totalOccupiedOnNode += usage
		}

		freeNPU := allocatable.Value() - totalOccupiedOnNode

		if freeNPU >= int64(reqCount) {
			bestNode = node.Name
			klog.V(4).InfoS("ARCSync: Potential node found for upcoming workflow", "node", bestNode, "free", freeNPU)
			break
		}
	}

	if bestNode == "" {
		klog.InfoS("ARCSync: PreFilter rejected pod (no node has enough slots after de-duplication)",
			"pod", pod.Name, "required", reqCount)
		return nil, framework.NewStatus(framework.Unschedulable, "No single node has enough available NPU slots")
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
