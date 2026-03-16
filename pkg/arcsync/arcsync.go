package arcsync

import (
	"context"
	"strconv"
	"sync"

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

type ARCSync struct {
	handle framework.Handle
	// key: nodeName, value: map[podUID]npuCount
	// 用于记录当前正在调度中（已过 PreFilter 但还没出现在节点快照中）的 NPU 预留量
	reservedNPU map[string]map[string]int64
	mu          sync.Mutex
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.ReservePlugin = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{
		handle:      h,
		reservedNPU: make(map[string]map[string]int64),
	}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

// PreFilter 阶段：检查是否有节点能满足需求，并提前“预占”资源
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

	foundNodeName := ""
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		// 1. 统计物理占用，并记录当前已存在的 Pod UID
		var occupiedNPU int64 = 0
		existingPodUIDs := make(map[string]bool)
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}
			existingPodUIDs[string(p.UID)] = true

			var podUsage int64 = 0
			if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(countStr, 10, 64)
					podUsage = count
				}
			}
			var requestUsage int64 = 0
			for _, container := range p.Spec.Containers {
				if res, ok := container.Resources.Requests[fullResourceName]; ok {
					requestUsage += res.Value()
				}
			}
			if requestUsage > podUsage {
				podUsage = requestUsage
			}
			occupiedNPU += podUsage
		}

		// 2. 统计账本中的预留量，并进行自愈校准
		// 如果账本中的 Pod 已经出现在了 Snapshot (NodeInfo.Pods) 中，则说明它已从“飞行中”落地
		// 此时应从账本中移除该 Pod 的预留，防止重复计数
		var reservedNPUOnNode int64 = 0
		if nodeReserved, ok := pl.reservedNPU[node.Name]; ok {
			for podUID, count := range nodeReserved {
				if existingPodUIDs[podUID] {
					// 自愈：该 Pod 已出现在物理快照中，移除逻辑预留
					klog.V(4).InfoS("ARCSync: Self-healing: removing pod from reservedNPU as it is now in snapshot", "node", node.Name, "podUID", podUID, "count", count)
					delete(nodeReserved, podUID)
					continue
				}
				reservedNPUOnNode += count
			}
		}

		allocatableNPU := node.Status.Allocatable[fullResourceName]
		freeNPU := allocatableNPU.Value() - occupiedNPU - reservedNPUOnNode

		klog.V(4).InfoS("ARCSync: Node status in PreFilter", "pod", pod.Name, "node", node.Name, "allocatable", allocatableNPU.Value(), "occupied", occupiedNPU, "reserved", reservedNPUOnNode, "free", freeNPU)

		if freeNPU >= int64(reqCount) {
			foundNodeName = node.Name
			break
		}
	}

	if foundNodeName != "" {
		// 记录预占，Key 使用 Pod 的 UID (或者名字+命名空间作为唯一标识)
		// 注意：如果 Pod 还没有 UID (极少见于 PreFilter)，可以使用 Pod 名字
		podKey := string(pod.UID)
		if podKey == "" {
			podKey = pod.Namespace + "/" + pod.Name
		}

		if _, ok := pl.reservedNPU[foundNodeName]; !ok {
			pl.reservedNPU[foundNodeName] = make(map[string]int64)
		}
		pl.reservedNPU[foundNodeName][podKey] = int64(reqCount)

		klog.InfoS("ARCSync: Pre-reserved NPU in PreFilter", "pod", pod.Name, "node", foundNodeName, "count", reqCount)
		return nil, framework.NewStatus(framework.Success, "")
	}

	klog.InfoS("ARCSync: PreFilter rejected pod (insufficient resources)", "pod", pod.Name, "required", reqCount)
	return nil, framework.NewStatus(framework.Unschedulable, "Insufficient global NPU resources")
}

// Reserve 阶段
func (pl *ARCSync) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	// 在目前的 PreFilter 机制下，Reserve 主要作为确认记录
	klog.V(4).InfoS("ARCSync: Reserve confirmed on node", "pod", pod.Name, "node", nodeName)
	return nil
}

// Unreserve 阶段
func (pl *ARCSync) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}

	if nodeReserved, ok := pl.reservedNPU[nodeName]; ok {
		if _, exists := nodeReserved[podKey]; exists {
			delete(nodeReserved, podKey)
			klog.InfoS("ARCSync: Unreserved NPU on node", "pod", pod.Name, "node", nodeName)
		}
	}
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
