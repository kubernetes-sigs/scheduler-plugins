package gpumanagementfilter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func TestGPUManagementFilter(t *testing.T) {
	tests := []struct {
		msg        string
		pod        *v1.Pod
		node       *framework.NodeInfo
		wantStatus *framework.Status
	}{
		{
			msg:        "gpu node, gpu pod",
			pod:        makeGPUPod("p1", 2),
			node:       makeGPUNode("n1", "2"),
			wantStatus: nil,
		},
		{
			msg:        "non-gpu node, gpu pod",
			pod:        makeGPUPod("p2", 2),
			node:       makeNonGPUNode("n2"),
			wantStatus: nil,
		},
		{
			msg:        "gpu node, non-gpu pod",
			pod:        makeNonGPUPod("p3"),
			node:       makeGPUNode("n3", "2"),
			wantStatus: framework.NewStatus(framework.Unschedulable, "gpu node, non-gpu pod"),
		},
		{
			msg:        "non-gpu node, non-gpu pod",
			pod:        makeNonGPUPod("p4"),
			node:       makeNonGPUNode("n4"),
			wantStatus: nil,
		},
		{
			msg:        "gpu node, gpu pod (requesting 0 GPUs)",
			pod:        makeGPUPod("p5", 0),
			node:       makeGPUNode("n5", "2"),
			wantStatus: framework.NewStatus(framework.Unschedulable, "gpu node, non-gpu pod"),
		},
		{
			msg:        "non-gpu node, gpu pod (requesting 0 GPUs)",
			pod:        makeGPUPod("p6", 0),
			node:       makeNonGPUNode("n6"),
			wantStatus: nil,
		},
		{
			msg:        "gpu node, device plugin pod",
			pod:        makeNonGPUNvidiaDevicePluginPod("p7"),
			node:       makeNonGPUNode("n7"),
			wantStatus: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			ctx := context.Background()
			cycleState := framework.NewCycleState()
			plugin := &GPUManagementFilter{
				profileName: v1.DefaultSchedulerName,
			}
			if _, status := plugin.PreFilter(ctx, cycleState, tt.pod); !status.IsSuccess() {
				t.Fatal(status.AsError())
			}

			status := plugin.Filter(ctx, cycleState, tt.pod, tt.node)
			assert.Equal(t, tt.wantStatus, status)
		})
	}
}

func makeGPUPod(node string, limits int64) *v1.Pod {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			NodeName: node,
			Containers: []v1.Container{v1.Container{Resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{v1.ResourceName(ResourceNvidiaGPU): *resource.NewQuantity(limits, resource.DecimalSI)},
			}}},
		},
	}
	return p
}

func makeGPUNode(node, gpu string, pods ...*v1.Pod) *framework.NodeInfo {
	n := framework.NewNodeInfo(pods...)
	n.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceName(ResourceNvidiaGPU): resource.MustParse(gpu),
			},
		},
	})
	return n
}

func makeNonGPUPod(node string) *v1.Pod {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			NodeName:   node,
			Containers: []v1.Container{v1.Container{Resources: v1.ResourceRequirements{}}},
		},
	}
	return p
}

func makeNonGPUNvidiaDevicePluginPod(node string) *v1.Pod {
	p := &v1.Pod{
		Spec: v1.PodSpec{
			NodeName: node,
			Containers: []v1.Container{
				v1.Container{
					Image:     "nvcr.io/nvidia/k8s-device-plugin:v0.11.0",
					Resources: v1.ResourceRequirements{},
				},
			},
		},
	}
	return p
}

func makeNonGPUNode(node string, pods ...*v1.Pod) *framework.NodeInfo {
	n := framework.NewNodeInfo(pods...)
	n.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{},
		},
	})
	return n
}
