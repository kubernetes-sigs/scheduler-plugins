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
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	klog.InfoS("ARCSync: Global resource pre-check", "pod", pod.Name, "requiredNPU", reqCount)

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

		var occupiedNPU int64 = 0
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
				continue
			}

			if countStr, ok := p.Labels[AllocatedNPUCount]; ok {
				// 只有当资源类型标签匹配时才累加
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(countStr, 10, 64)
					occupiedNPU += count
				}
			} else {
				for _, container := range p.Spec.Containers {
					if res, ok := container.Resources.Requests[fullResourceName]; ok {
						occupiedNPU += res.Value()
					}
				}
			}
		}

		allocatableNPU := node.Status.Allocatable[fullResourceName]
		if allocatableNPU.Value()-occupiedNPU >= int64(reqCount) {
			foundCandidate = true
			break
		}
	}

	if foundCandidate {
		return nil, framework.NewStatus(framework.Success, "")
	}

	return nil, framework.NewStatus(framework.Unschedulable, "Insufficient global NPU resources")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}
