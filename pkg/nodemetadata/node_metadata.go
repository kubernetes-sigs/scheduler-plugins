/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodemetadata

import (
	"context"
	"fmt"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/validation"
)

// NodeMetadata is a plugin that scores nodes based on their metadata (labels or annotations)
// containing numeric values or timestamps.
type NodeMetadata struct {
	logger klog.Logger
	handle framework.Handle
	args   *config.NodeMetadataArgs
}

// Ensure NodeMetadata implements the ScorePlugin interface at compile time
var _ framework.ScorePlugin = &NodeMetadata{}

// Name is the name of the plugin used in the Registry and configurations.
const Name = "NodeMetadata"

// Name returns the name of the plugin.
func (nm *NodeMetadata) Name() string {
	return Name
}

// Score invoked at the score extension point.
// Scores nodes based on metadata (label or annotation) containing numeric or timestamp values.
func (nm *NodeMetadata) Score(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) (int64, *fwk.Status) {
	logger := klog.FromContext(klog.NewContext(ctx, nm.logger)).WithValues("ExtensionPoint", "Score")
	node := nodeInfo.Node()
	if node == nil {
		return 0, fwk.NewStatus(fwk.Error, fmt.Sprintf("node %q not found", nodeInfo.Node().Name))
	}

	score, err := nm.calculateScore(node)
	if err != nil {
		logger.V(5).Info("Failed to calculate score for node", "node", node.Name, "error", err, "pod", pod.Name)
		// Return 0 score for nodes where we can't calculate the score
		return 0, nil
	}

	logger.V(10).Info("Score: ", "score", score, "node", node.Name, "pod", pod.Name)
	return score, nil
}

// ScoreExtensions of the Score plugin.
func (nm *NodeMetadata) ScoreExtensions() framework.ScoreExtensions {
	return nm
}

// calculateScore computes the raw score for a node based on its metadata
func (nm *NodeMetadata) calculateScore(node *v1.Node) (int64, error) {
	var metadataValue string
	var found bool

	// Get the metadata value from label or annotation
	if nm.args.MetadataSource == config.MetadataSourceLabel {
		metadataValue, found = node.Labels[nm.args.MetadataKey]
	} else {
		metadataValue, found = node.Annotations[nm.args.MetadataKey]
	}

	if !found {
		return 0, fmt.Errorf("metadata key %q not found in %s", nm.args.MetadataKey, nm.args.MetadataSource)
	}

	// Parse the value based on the configured type
	switch nm.args.MetadataType {
	case config.MetadataTypeNumber:
		return nm.parseNumericValue(metadataValue)
	case config.MetadataTypeTimestamp:
		return nm.parseTimestampValue(metadataValue)
	default:
		return 0, fmt.Errorf("unsupported metadata type: %s", nm.args.MetadataType)
	}
}

// parseNumericValue parses a numeric value from metadata
func (nm *NodeMetadata) parseNumericValue(value string) (int64, error) {
	numValue, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse numeric value %q: %w", value, err)
	}

	// Convert to int64 with optional inversion based on scoring strategy
	score := int64(numValue)
	if nm.args.ScoringStrategy == config.ScoringStrategyLowest {
		// Invert: lower values should get higher scores
		score = -score
	}
	// For ScoringStrategyHighest, higher values naturally get higher scores

	return score, nil
}

// parseTimestampValue parses a timestamp value and converts it to a score
func (nm *NodeMetadata) parseTimestampValue(value string) (int64, error) {
	// Try parsing with the configured format
	timestamp, err := time.Parse(nm.args.TimestampFormat, value)
	if err != nil {
		return 0, fmt.Errorf("failed to parse timestamp %q with format %q: %w", value, nm.args.TimestampFormat, err)
	}

	// Calculate age in seconds
	age := time.Since(timestamp).Seconds()

	var score int64
	if nm.args.ScoringStrategy == config.ScoringStrategyNewest {
		// Newer timestamps (smaller age) should get higher scores
		// Use negative age so newer = less negative = higher after normalization
		score = -int64(age)
	} else {
		// Older timestamps (larger age) should get higher scores
		score = int64(age)
	}

	return score, nil
}

// NormalizeScore normalizes the scores across all nodes to fit within the framework's score range.
func (nm *NodeMetadata) NormalizeScore(ctx context.Context, state fwk.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *fwk.Status {
	logger := klog.FromContext(klog.NewContext(ctx, nm.logger)).WithValues("ExtensionPoint", "NormalizeScore")
	logger.V(10).Info("Original scores: ", "scores", scores, "pod", pod.Name)
	// Find min and max scores
	if len(scores) == 0 {
		return nil
	}

	var minScore, maxScore int64
	minScore = scores[0].Score
	maxScore = scores[0].Score

	for i := range scores {
		if scores[i].Score < minScore {
			minScore = scores[i].Score
		}
		if scores[i].Score > maxScore {
			maxScore = scores[i].Score
		}
	}

	logger.V(10).Info("Score range: ", "min", minScore, "max", maxScore, "pod", pod.Name)

	// If all scores are the same, set them all to MinNodeScore
	if maxScore == minScore {
		for i := range scores {
			scores[i].Score = framework.MinNodeScore
		}
		logger.V(10).Info("All scores equal, normalized to MinNodeScore: ", "scores", scores, "pod", pod.Name)
		return nil
	}

	// Normalize scores to [MinNodeScore, MaxNodeScore] range
	oldRange := maxScore - minScore
	newRange := framework.MaxNodeScore - framework.MinNodeScore

	for i := range scores {
		scores[i].Score = ((scores[i].Score-minScore)*newRange)/oldRange + framework.MinNodeScore
	}

	logger.V(10).Info("Normalized scores: ", "scores", scores, "pod", pod.Name)
	return nil
}

// New initializes a new plugin and returns it.
func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)

	args, ok := obj.(*config.NodeMetadataArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NodeMetadataArgs, got %T", obj)
	}

	// Validate arguments
	if err := validation.ValidateNodeMetadataArgs(args, nil); err != nil {
		return nil, fmt.Errorf("invalid NodeMetadataArgs: %w", err)
	}

	return &NodeMetadata{
		logger: logger,
		handle: h,
		args:   args,
	}, nil
}
