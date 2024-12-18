package gpumanagementfilter

import v1 "k8s.io/api/core/v1"

// RequestsNvidiaGPU checks if any container in the list requests for a NVIDIA GPU
func requestsNvidiaGPU(containerList []v1.Container) bool {
	for _, containerItem := range containerList {
		if checkNvidiaGPUResources(containerItem.Resources.Requests) || checkNvidiaGPUResources(containerItem.Resources.Limits) {
			return true
		}
	}
	return false
}

func checkNvidiaGPUResources(containerResources v1.ResourceList) bool {
	value, isPresent := containerResources[ResourceNvidiaGPU]
	valueInt, _ := value.AsInt64()
	if isPresent && valueInt > 0 {
		return true
	}
	return false
}
