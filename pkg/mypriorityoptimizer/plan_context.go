// plan_context.go
package mypriorityoptimizer

import (
	v1 "k8s.io/api/core/v1"
)

// planContext gathers the current cluster view, builds solver input,
// computes the baseline score, and counts pending pods.
func (pl *SharedState) planContext(preemptor *v1.Pod) (
	nodes []*v1.Node,
	pods []*v1.Pod,
	pendingPrePlan int,
	inp SolverInput,
	err error,
) {
	// Fetch nodes
	nodes, err = pl.getNodes()
	if err != nil {
		return nil, nil, 0, SolverInput{}, ErrFailedToListNodes
	}

	// Fetch pods
	pods, err = pl.getPods()
	if err != nil {
		return nodes, nil, 0, SolverInput{}, ErrFailedToListPods
	}
	// Count pending pods before any plan is applied
	pendingPrePlan = countPendingPods(pods)
	if pendingPrePlan == 0 {
		return nodes, pods, pendingPrePlan, SolverInput{}, ErrNoPendingPods
	}

	// Build solver input for this snapshot
	inp, err = pl.buildSolverInput(nodes, pods, preemptor)
	if err != nil {
		return nodes, pods, pendingPrePlan, SolverInput{}, ErrFailedToBuildSolverInput
	}

	return nodes, pods, pendingPrePlan, inp, nil
}
