// run_flow.go

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// runFlow runs the flow for the given phase (AllSynch, AllAsynch, Single).
// For Single phase, the preemptor must be provided.
// Returns the target node name for the preemptor pod (if any) and error (if any).
func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, preemptor *v1.Pod) (*Plan, error) {

	strategy := strategyToString()

	// Continuous: do NOT take Active yet (first take it after solver has the plan and there is an improvement to apply).
	// Batch/Single: take Active early because these modes block by design.
	if !optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.V(MyV).InfoS(msg(strategy, InfoActivePlanInProgress))
			return nil, ErrActiveInProgress
		}
	}

	start := time.Now()

	// Fetch nodes and pods ONCE for this flow
	nodes, err := pl.getNodes()
	if err != nil {
		pl.leaveActive()
		klog.Error(msg(strategy, "failed to list nodes"))
		return nil, err
	}
	pods, err := pl.getPods()
	if err != nil {
		pl.leaveActive()
		klog.Error(msg(strategy, "failed to list pods"))
		return nil, err
	}

	// Count pending pods
	pendingPostPlan := countPendingPods(pods)
	if pendingPostPlan == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPods))
		pl.leaveActive()
		return nil, ErrNoop
	}

	// Run solvers
	solverInput, err := pl.buildSolverInput(nodes, pods, preemptor)
	if err != nil {
		klog.Error(msg(strategy, "failed to build solver input"))
		pl.leaveActive()
		return nil, err
	}
	bestSolver, anyFeasible := pl.runSolvers(ctx, solverInput, nodes, pods)
	// Check if all solvers are infeasible -> ErrNoOptimalOrFeasible
	if !anyFeasible {
		pl.leaveActive()
		klog.Error(msg(strategy, InfoNoSolverSolution))
		return nil, ErrNoSolverSolution
	}

	// Take Active late for AllSynch (only now that we know it's worth applying a plan).
	if optimizeAllAsynch() {
		if !pl.tryEnterActive() {
			klog.InfoS(msg(strategy, InfoActivePlanInProgress))
			return nil, ErrActiveInProgress
		}
	}

	// Count new and total pods. If no pending pods to be scheduled -> ErrNoop
	pendingScheduled, totalPrePlan, totalPostPlan := pl.countNewAndTotalPods(bestSolver.Output, pods)
	if pendingScheduled == 0 {
		klog.InfoS(msg(strategy, InfoNoPendingPodsToSchedule))
		pl.leaveActive()
		return nil, ErrNoop
	}

	// Register and execute storedPlan
	plan, ap, err := pl.registerPlan(ctx, bestSolver, preemptor, pods)
	if err != nil {
		klog.Error(msg(strategy, InfoRegisterPlanFailed))
		pl.leaveActive()
		return nil, ErrRegisterPlan
	}

	// Execute if there are moves/evictions
	if err := pl.executePlan(plan); err != nil {
		klog.Error(msg(strategy, InfoPlanExecutionFailed))
		pl.onPlanSettled(PlanStatusFailed)
	}

	// If in all modes activate planned pending pods (now that the plan is in place).
	if optimizeAllSynch() || optimizeAllAsynch() || optimizeManualHttp() {
		pl.activatePlannedPending(plan, pods)
	}

	// Build and return result
	bestSolverSummary := summarizeAttempt(bestSolver)
	klog.InfoS(msg(strategy, InfoPlanExecutionFinished),
		"planID", ap.ID,
		"bestSolver", bestSolverSummary,
		"pendingPostPlan", pendingPostPlan,
		"pendingScheduled", pendingScheduled,
		"totalPrePlan", totalPrePlan,
		"totalPostPlan", totalPostPlan,
		"totalDuration", time.Since(start),
	)

	// Return the stored plan for inspection (if needed)
	return plan, nil
}
