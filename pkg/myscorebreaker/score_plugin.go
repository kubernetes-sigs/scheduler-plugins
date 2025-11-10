package myscorebreaker

import (
	"context"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	framework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// Name of  plugin
const Name = "MyScoreBreaker"

// Ensure our plugin implements both ScorePlugin and ScoreExtensions.
var (
	_ framework.ScorePlugin     = &Plugin{}
	_ framework.ScoreExtensions = &Plugin{}
)

type Plugin struct{}

// New initializes the plugin.
func New(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
	return &Plugin{}, nil
}

func (p *Plugin) Name() string { return Name }

// Score returns 0 for every node (we do the ordering in NormalizeScore).
func (p *Plugin) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	return 0, nil
}

// We expose ScoreExtensions to get NormalizeScore.
func (p *Plugin) ScoreExtensions() framework.ScoreExtensions { return p }

// NormalizeScore imposes a deterministic order by node name.
// Higher score wins. With scheduler weight=1, this acts as a gentle tie-break.
func (p *Plugin) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Sort nodes lexicographically by name (stable & deterministic).
	sort.SliceStable(scores, func(i, j int) bool {
		return scores[i].Name < scores[j].Name
	})

	// Assign a strictly increasing score across the sorted list.
	// Scale into [0, MaxNodeScore] so the plugin has full internal resolution,
	n := len(scores)
	if n <= 1 {
		return framework.NewStatus(framework.Success, "")
	}

	for i := range scores {
		// Example: n=4 -> indices [0..3]
		desc := int64(framework.MaxNodeScore) * int64(n-1-i) / int64(n-1)
		scores[i].Score = desc
	}
	return framework.NewStatus(framework.Success, "")
}
