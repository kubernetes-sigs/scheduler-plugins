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
	// 用于记录当前正在调度中（已过 PreFilter 但还没完成绑定或失败）的 NPU 预留量
	// key: nodeName, value: reserved NPU count
	reservedNPU map[string]int64
	mu          sync.Mutex
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.ReservePlugin = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{
		handle:      h,
		reservedNPU: make(map[string]int64),
	}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

// PreFilter 阶段：检查是否有节点能满足需求
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

	foundCandidate := false
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		// 1. 统计物理占用
		var occupiedNPU int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}

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

		// 2. 加上插件内部记录的“正在调度中”的预留量
		reserved := pl.reservedNPU[node.Name]

		allocatableNPU := node.Status.Allocatable[fullResourceName]
		freeNPU := allocatableNPU.Value() - occupiedNPU - reserved

		if freeNPU >= int64(reqCount) {
			foundCandidate = true
			klog.InfoS("ARCSync: PreFilter allowed pod", "pod", pod.Name, "node", node.Name, "free", freeNPU, "reserved", reserved)
			break
		}
	}

	if foundCandidate {
		return nil, framework.NewStatus(framework.Success, "")
	}

	klog.InfoS("ARCSync: PreFilter rejected pod (insufficient resources)", "pod", pod.Name, "required", reqCount)
	return nil, framework.NewStatus(framework.Unschedulable, "Insufficient global NPU resources")
}

// Reserve 阶段：当调度器决定将 Pod 尝试放在某个节点时触发
func (pl *ARCSync) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return nil
	}
	reqCount, _ := strconv.Atoi(reqCountStr)

	pl.mu.Lock()
	defer pl.mu.Unlock()

	pl.reservedNPU[nodeName] += int64(reqCount)
	klog.InfoS("ARCSync: Reserved NPU on node", "pod", pod.Name, "node", nodeName, "count", reqCount, "totalReserved", pl.reservedNPU[nodeName])
	return nil
}

// Unreserve 阶段：当 Pod 调度成功（绑定完成）或调度失败时触发，释放预留
func (pl *ARCSync) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return
	}
	reqCount, _ := strconv.Atoi(reqCountStr)

	pl.mu.Lock()
	defer pl.mu.Unlock()

	pl.reservedNPU[nodeName] -= int64(reqCount)
	if pl.reservedNPU[nodeName] < 0 {
		pl.reservedNPU[nodeName] = 0
	}
	klog.InfoS("ARCSync: Unreserved NPU on node", "pod", pod.Name, "node", nodeName, "count", reqCount, "remainingReserved", pl.reservedNPU[nodeName])
}


func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
