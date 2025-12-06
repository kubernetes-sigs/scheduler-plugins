// plan_context.go
package mypriorityoptimizer

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
)

// planContext gathers the current cluster view, builds solver input,
// computes the baseline score, and counts pending pods.
func (pl *SharedState) planContext(preemptor *v1.Pod) (
	[]*v1.Node,
	[]*v1.Pod,
	SolverInput,
	*SolverScore,
	int, // pendingPrePlan
	error,
) {
	// Fetch nodes
	nodes, err := pl.getNodes()
	if err != nil {
		var zeroInp SolverInput
		return nil, nil, zeroInp, nil, 0, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Fetch pods
	pods, err := pl.getPods()
	if err != nil {
		var zeroInp SolverInput
		return nil, nil, zeroInp, nil, 0, fmt.Errorf("failed to list pods: %w", err)
	}

	// Build solver input for this snapshot
	inp, err := pl.buildSolverInput(nodes, pods, preemptor) // inp is SolverInput
	if err != nil {
		var zeroInp SolverInput
		return nil, nil, zeroInp, nil, 0, fmt.Errorf("failed to build solver input: %w", err)
	}

	// Compute baseline score
	baselineScore := buildBaselineScore(inp)

	// Count pending pods before any plan is applied
	pendingPrePlan := countPendingPods(pods)

	return nodes, pods, inp, baselineScore, pendingPrePlan, nil
}
