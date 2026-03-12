package arcsync

import (
	"context"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/klog/v2"
)

const (
	// cache-buster: 2026-03-12-03-00
	Name = "ARCSync"
	// Runner Pod 标签：声明后续 Workflow Pod 需要的资源
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"

	// Workflow Pod (已存在) 上的标识标签
	AllocatedNPUCount = "ascend-ci.com/npu-count"
)

type ARCSync struct {
	handle framework.Handle
}

// 确保插件实现了 PreFilter 接口
var _ framework.PreFilterPlugin = &ARCSync{}

func New(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{handle: h}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

// PreFilter 阶段：全局资源预检
func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// 1. 检查是否为需要拦截的 Runner Pod
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		// 如果没有该标签，说明不是需要同步的 Runner，直接放行
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]

	// 拼接完整的资源名称，例如 "huawei.com/ascend-310"
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	klog.InfoS("ARCSync: Global resource pre-check for runner pod",
		"pod", pod.Name, "requiredNPU", reqCount, "resource", fullResourceName)

	// 2. 获取集群中所有节点的快照
	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	// 3. 遍历所有节点，寻找是否有“至少一个”节点满足未来的 Workflow Pod
	foundCandidate := false
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		// 计算该节点上已经被占用的 NPU 总数
		var occupiedNPU int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			// 忽略已终止的 Pod (Succeeded/Failed)
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}

			// 逻辑 A: 优先读取标签声明的 NPU 数目 (用于识别特定的 Workflow Pod)
			if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
				count, _ := strconv.ParseInt(countStr, 10, 64)
				occupiedNPU += count
			} else {
				// 逻辑 B: 检查 Request 资源分配 (没有标签但实际占用了资源)
				for _, container := range p.Spec.Containers {
					if res, ok := container.Resources.Requests[fullResourceName]; ok {
						occupiedNPU += res.Value()
					}
				}
			}
		}

		// 获取该节点总的可分配 NPU 数量
		allocatableNPU := node.Status.Allocatable[fullResourceName]

		// 计算剩余可用资源
		freeNPU := allocatableNPU.Value() - occupiedNPU

		if freeNPU >= int64(reqCount) {
			klog.InfoS("ARCSync: Found a candidate node for potential workflow",
				"node", node.Name, "freeNPU", freeNPU, "requiredNPU", reqCount)
			foundCandidate = true
			break // 只要找到一个满足条件的节点，全局预检就通过
		}
	}

	// 4. 结论：如果没有任何节点能满足需求，则挂起 Runner Pod
	if foundCandidate {
		return nil, framework.NewStatus(framework.Success, "")
	}

	klog.InfoS("ARCSync: No nodes available for future workflow, holding runner pod",
		"pod", pod.Name, "requiredNPU", reqCount)
	return nil, framework.NewStatus(framework.Unschedulable, "Insufficient global NPU resources for future workflow pod")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
