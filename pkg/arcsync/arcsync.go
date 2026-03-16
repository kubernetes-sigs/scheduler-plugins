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

// ARCSync 插件不再需要维护全局内存账本，从而消除状态泄露风险
type ARCSync struct {
	handle framework.Handle
	mu     sync.Mutex
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.FilterPlugin = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{
		handle: h,
	}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

// PreFilter 阶段仅做基础检查
func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	if _, ok := pod.Labels[RequiredNPUCount]; !ok {
		return nil, framework.NewStatus(framework.Success, "")
	}
	return nil, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

// Filter 阶段：这是最严谨的检查点
func (pl *ARCSync) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	node := nodeInfo.Node()
	if node == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}

	// 1. 统计当前节点上已经存在的 Pod 资源占用（物理快照）
	// nodeInfo.Pods 包含了已经调度到该节点的所有 Pod（包括正在 Binding 的）
	var occupiedNPU int64 = 0
	for _, podInfo := range nodeInfo.Pods {
		p := podInfo.Pod
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}

		var podUsage int64 = 0
		// 优先从业务标签读取准确的 NPU 分配数
		if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
			if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
				count, _ := strconv.ParseInt(countStr, 10, 64)
				podUsage = count
			}
		}

		// 备选方案：从 Resource Requests 读取
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

	// 2. 获取节点总的可分配资源
	allocatableNPU, ok := node.Status.Allocatable[fullResourceName]
	if !ok {
		klog.V(3).InfoS("ARCSync: Node has no NPU resource", "node", node.Name, "resource", fullResourceName)
		return framework.NewStatus(framework.Unschedulable, "Node has no such NPU resource")
	}

	freeNPU := allocatableNPU.Value() - occupiedNPU

	klog.V(4).InfoS("ARCSync: Filter checking node",
		"pod", pod.Name,
		"node", node.Name,
		"allocatable", allocatableNPU.Value(),
		"occupied", occupiedNPU,
		"free", freeNPU,
		"required", reqCount)

	if freeNPU < int64(reqCount) {
		return framework.NewStatus(framework.Unschedulable, "Insufficient NPU resources on node")
	}

	return framework.NewStatus(framework.Success, "")
}
