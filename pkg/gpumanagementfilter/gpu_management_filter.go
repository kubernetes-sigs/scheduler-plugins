package gpumanagementfilter

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// PluginName is the name of the plugin used in the plugin registry and configurations.
	PluginName = "GPUManagementFilter"

	//preFilterStateKey is the key in CycleState to NodeResourcesFit pre-computed data.
	// Using the name of the plugin helps avoid collisions with other plugins.
	preFilterStateKey = "PreFilter" + PluginName

	//devicePluginContainerImage is the nvidia provided kubernetes device plugin image.
	devicePluginContainerImage = "nvcr.io/nvidia/k8s-device-plugin"

	// ResourceNvidiaGPU is nvidia GPU resource
	ResourceNvidiaGPU = "nvidia.com/gpu"
)

type GPUManagementFilter struct {
	profileName string
}

var (
	_ framework.FilterPlugin    = &GPUManagementFilter{}
	_ framework.PreFilterPlugin = &GPUManagementFilter{}
)

// Name returns name of the plugin. It is used in logs, etc.
func (p *GPUManagementFilter) Name() string {
	return PluginName
}

// preFilterState computed at PreFilter and used in Filter.
type preFilterState struct {
	isPodRequestingGPUs     bool
	isPodNvidiaDevicePlugin bool
}

// Clone implements the StateData interface.
// It does not actually copy the data, as there is no need to do so.
func (s *preFilterState) Clone() framework.StateData {
	return s
}

// PreFilter invoked at the prefilter extension point.
func (p *GPUManagementFilter) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	cycleState.Write(preFilterStateKey, updatePreFilterState(pod))
	return nil, nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (p *GPUManagementFilter) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func getPreFilterState(cycleState *framework.CycleState) (*preFilterState, error) {
	c, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		return nil, fmt.Errorf("error reading %q from cycleState: %w", preFilterStateKey, err)
	}

	s, ok := c.(*preFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to GPUSchedulerFilterPlugin.preFilterState error", c)
	}
	return s, nil
}

// Filter invoked at the filter extension point.
func (p *GPUManagementFilter) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	s, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}

	if (!s.isPodRequestingGPUs) && isGPUNode(nodeInfo) && (!s.isPodNvidiaDevicePlugin) {
		return framework.NewStatus(framework.Unschedulable, "gpu node, non-gpu pod")
	}

	return nil
}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &GPUManagementFilter{}, nil
}

// updatePreFilterState checks if given pod is requesting GPUs or not by checking resource limits it also checks if the pod is a Nvidia Device Plugin Pod or not and updates the PreFilter State accordingly
func updatePreFilterState(pod *v1.Pod) *preFilterState {
	result := &preFilterState{}

	initContainers := pod.Spec.InitContainers
	containers := pod.Spec.Containers

	if containsNvidiaDevicePluginImage(containers) {
		result.isPodNvidiaDevicePlugin = true
	}

	if requestsNvidiaGPU(initContainers) || requestsNvidiaGPU(containers) {
		result.isPodRequestingGPUs = true
		return result
	}
	return result
}

// isGPUNode checks if given node has GPU resource or not by checking Allocatable
func isGPUNode(nodeInfo *framework.NodeInfo) bool {
	_, gpuAllocatable := nodeInfo.Allocatable.ScalarResources[ResourceNvidiaGPU]
	return gpuAllocatable
}

// containsNvidiaDevicePluginImage checks if any of the container images in containerList are the nvidia device plugin. The device plugin image would be the invariant across different Daemonset specs (only the version would be changed). Labels or annotation might be changed or added by anyone on their pod spec
func containsNvidiaDevicePluginImage(containerList []v1.Container) bool {
	for _, containerItem := range containerList {
		if strings.Contains(containerItem.Image, devicePluginContainerImage) {
			return true
		}
	}
	return false
}
