package myplugin

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const Name = "MyPlugin"

// MyPlugin is a scheduler plugin that demonstrates basic functionality
type MyPlugin struct {
	logger klog.Logger
	handle framework.Handle
}

// Ensure MyPlugin implements the necessary interfaces
var _ framework.ScorePlugin = &MyPlugin{}
var _ framework.FilterPlugin = &MyPlugin{}

// New initializes a new plugin and returns it.
func New(ctx context.Context, args runtime.Object, h framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)

	// You can parse configuration arguments here if needed
	// if args != nil {
	//     // Parse your plugin configuration
	// }

	return &MyPlugin{
		logger: logger,
		handle: h,
	}, nil
}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *MyPlugin) Name() string {
	return Name
}

// Filter is called during the filtering phase to determine if a pod can be scheduled on a node
func (pl *MyPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	pl.logger.Info("🔍 MyPlugin Filter: Checking pod for node", "pod", pod.Name, "node", nodeInfo.Node().Name)

	// Example filter logic: reject nodes with specific labels
	if nodeInfo.Node().Labels["example.com/special"] == "false" {
		pl.logger.Info("❌ MyPlugin Filter: Node rejected (special=false)", "node", nodeInfo.Node().Name)
		return framework.NewStatus(framework.Unschedulable, "Node marked as not special")
	}

	pl.logger.Info("✅ MyPlugin Filter: Node accepted", "node", nodeInfo.Node().Name)
	// Allow the pod to be scheduled on this node
	return framework.NewStatus(framework.Success)
}

// Score is called during the scoring phase to rank nodes
func (pl *MyPlugin) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	pl.logger.Info("� MyPlugin Score: HOT RELOAD V3 TESTED! - Node evaluation in progress", "pod", pod.Name, "node", nodeName)

	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	score := int64(0)

	// Example scoring logic: prefer nodes with more CPU available
	allocatedCPU := nodeInfo.Allocatable.MilliCPU - nodeInfo.Requested.MilliCPU
	if allocatedCPU > 0 {
		score = allocatedCPU / 100 // Convert to a reasonable score range
		if score > 100 {
			score = 100 // Cap the score
		}
	}

	pl.logger.Info("🏆 MyPlugin Score: Calculated score", "pod", pod.Name, "node", nodeName, "score", score, "availableCPU", allocatedCPU)
	return score, framework.NewStatus(framework.Success)
}

// ScoreExtensions returns the score extensions for this plugin
func (pl *MyPlugin) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// NormalizeScore is called after all nodes have been scored
func (pl *MyPlugin) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find the highest score
	var highest int64 = 0
	for _, nodeScore := range scores {
		if nodeScore.Score > highest {
			highest = nodeScore.Score
		}
	}

	// Normalize scores to 0-100 range
	if highest > 0 {
		for i, nodeScore := range scores {
			scores[i].Score = (nodeScore.Score * framework.MaxNodeScore) / highest
		}
	}

	return framework.NewStatus(framework.Success)
}
