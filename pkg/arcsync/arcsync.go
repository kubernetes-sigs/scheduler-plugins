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
	Name = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
)

type ARCSync struct {
	handle framework.Handle
}

var _ framework.PreFilterPlugin = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &ARCSync{handle: h}, nil
}

func (pl *ARCSync) Name() string {
	return Name
}

func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		// 如果没有声明 NPU 需求，我们不拦截，但为了排查方便，打印一条 DEBUG 日志
		klog.V(3).InfoS("ARCSync: Pod has no NPU requirement label, skipping", "pod", pod.Name)
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, err := strconv.Atoi(reqCountStr)
	if err != nil {
		klog.ErrorS(err, "ARCSync: Invalid required NPU count", "pod", pod.Name, "value", reqCountStr)
		return nil, framework.NewStatus(framework.Error, "invalid required-npu-count")
	}

	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	klog.InfoS("ARCSync: Starting global resource pre-check",
		"pod", pod.Name,
		"requiredNPU", reqCount,
		"resourceName", fullResourceName)

	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		klog.ErrorS(err, "ARCSync: Failed to get node snapshots", "pod", pod.Name)
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	foundCandidate := false
	var totalFreeNPU int64 = 0

	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}

		// 获取该节点物理分配量
		allocatableNPU := node.Status.Allocatable[fullResourceName]
		allocatableVal := allocatableNPU.Value()

		var occupiedNPU int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			// 只要不是终端状态，就认为占着资源
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}

			var podUsage int64 = 0
			// 1. 优先看标签（Workflow Pod 可能会标记这个）
			if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(countStr, 10, 64)
					podUsage = count
				}
			}

			// 2. 检查 Request (物理占用兜底)
			var requestUsage int64 = 0
			for _, container := range p.Spec.Containers {
				if res, ok := container.Resources.Requests[fullResourceName]; ok {
					requestUsage += res.Value()
				}
			}

			// 取较大值，确保统计保守
			if requestUsage > podUsage {
				podUsage = requestUsage
			}

			if podUsage > 0 {
				occupiedNPU += podUsage
				klog.V(4).InfoS("ARCSync: Node pod usage detail",
					"node", node.Name,
					"pod", p.Name,
					"phase", p.Status.Phase,
					"usage", podUsage)
			}
		}

		freeNPU := allocatableVal - occupiedNPU
		if freeNPU < 0 {
			freeNPU = 0
		}
		totalFreeNPU += freeNPU

		klog.InfoS("ARCSync: Node NPU Status",
			"node", node.Name,
			"allocatable", allocatableVal,
			"occupied", occupiedNPU,
			"free", freeNPU)

		if freeNPU >= int64(reqCount) {
			foundCandidate = true
			klog.InfoS("ARCSync: Candidate node found",
				"pod", pod.Name,
				"node", node.Name,
				"needed", reqCount,
				"has", freeNPU)
			// 为了看全集群状态，我们不 break，继续打印完所有节点日志
			// break
		}
	}

	if foundCandidate {
		klog.InfoS("ARCSync: PreFilter passed", "pod", pod.Name, "totalFreeClusterNPU", totalFreeNPU)
		return nil, framework.NewStatus(framework.Success, "")
	}

	klog.InfoS("ARCSync: PreFilter FAILED - Insufficient resources",
		"pod", pod.Name,
		"required", reqCount,
		"totalFreeClusterNPU", totalFreeNPU)

	return nil, framework.NewStatus(framework.Unschedulable, "Insufficient global NPU resources for upcoming workflow")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
