package myplugin

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

func TestMyPlugin_Filter(t *testing.T) {
	tests := []struct {
		name     string
		node     *v1.Node
		expected framework.Code
	}{
		{
			name: "node with special label true should pass",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						"example.com/special": "true",
					},
				},
			},
			expected: framework.Success,
		},
		{
			name: "node with special label false should fail",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						"example.com/special": "false",
					},
				},
			},
			expected: framework.Unschedulable,
		},
		{
			name: "node without special label should pass",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
			},
			expected: framework.Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake client and informer factory
			ctx := context.Background()
			fh, _ := runtime.NewFramework(ctx, nil, nil)

			plugin, err := New(ctx, nil, fh)
			if err != nil {
				t.Fatalf("Failed to create plugin: %v", err)
			}

			myPlugin := plugin.(*MyPlugin)

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
			}

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(tt.node)

			status := myPlugin.Filter(ctx, nil, pod, nodeInfo)
			if status.Code() != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, status.Code())
			}
		})
	}
}

func TestMyPlugin_Score(t *testing.T) {
	ctx := context.Background()
	fh, _ := runtime.NewFramework(ctx, nil, nil)

	plugin, err := New(ctx, nil, fh)
	if err != nil {
		t.Fatalf("Failed to create plugin: %v", err)
	}

	myPlugin := plugin.(*MyPlugin)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
	}

	// Note: In a real test, you would need to properly set up the SharedLister
	// with node information. This is a simplified example.
	score, status := myPlugin.Score(ctx, nil, pod, "test-node")

	if status.Code() != framework.Success {
		t.Errorf("Expected Success, got %v", status.Code())
	}

	if score < 0 {
		t.Errorf("Expected non-negative score, got %d", score)
	}
}
