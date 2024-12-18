package gpumanagementfilter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var cpuResourceList = v1.ResourceList{
	"cpu": *resource.NewQuantity(5, resource.DecimalSI),
}

var gpuResourceList = v1.ResourceList{
	ResourceNvidiaGPU: *resource.NewQuantity(5, resource.DecimalSI),
}

var zeroGpuResourceList = v1.ResourceList{
	ResourceNvidiaGPU: *resource.NewQuantity(0, resource.DecimalSI),
}

func TestRequestsNvidiaGPU(t *testing.T) {
	tests := []struct {
		name         string
		resourceList v1.ResourceList
		expected     bool
	}{
		{
			name:         "containers requesting CPUs only",
			resourceList: cpuResourceList,
			expected:     false,
		},
		{
			name:         "containers requesting GPU resource with 0 value",
			resourceList: zeroGpuResourceList,
			expected:     false,
		},
		{
			name:         "containers requesting GPUs",
			resourceList: gpuResourceList,
			expected:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, requestsNvidiaGPU(getContainerList(tt.resourceList)))
		})
	}
}

func getContainerList(resourceList v1.ResourceList) []v1.Container {
	return []v1.Container{
		{
			Resources: v1.ResourceRequirements{
				Limits: resourceList,
			},
		},
		{
			Resources: v1.ResourceRequirements{
				Limits: cpuResourceList,
			},
		},
	}
}
