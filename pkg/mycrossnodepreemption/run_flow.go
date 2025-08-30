package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (pl *MyCrossNodePreemption) runFlow(ctx context.Context, phase Phase, singlePod *v1.Pod) (*FlowResult, error) {
	if !pl.tryEnterActive() {
		return nil, ErrActiveInProgress
	}

	var batchedPods []*v1.Pod
	if phase == PhaseBatch {
		_ = pl.pruneStaleSetEntries(pl.Batched) // Prune stale entries; keep only pending batched pods
		batchedPods = pl.snapshotBatch()
		if len(batchedPods) == 0 {
			pl.leaveActive()
			klog.InfoS(string(phase) + ": nothing to do")
			return &FlowResult{}, nil
		}
	}

	start := time.Now()
	ctxSolve, cancelTimeout := context.WithTimeout(ctx, SolverTimeout)
	var out *SolverOutput
	var err error
	if phase == PhaseBatch {
		out, err = pl.solve(ctxSolve, SolveBatch, nil, batchedPods, SolverTimeout)
	} else {
		out, err = pl.solve(ctxSolve, SolveSingle, singlePod, nil, SolverTimeout)
	}
	solverDur := time.Since(start)
	cancelTimeout()

	if err != nil || out == nil || (phase != PhaseBatch && out.NominatedNode == "") {
		pl.leaveActive()
		err = ErrSolver
		klog.ErrorS(err, string(phase)+": solver failed")
		return nil, err
	}

	plan, ap, err := pl.registerPlan(ctx, out, nil)
	if err != nil {
		if phase != PhaseBatch {
			pl.Blocked.AddPod(singlePod)
		}
		pl.leaveActive()
		klog.ErrorS(err, string(phase)+": register plan failed")
		err = ErrRegisterPlan
		return nil, err
	}

	_ = pl.executePlanIfOps(ctx, plan)

	// activate everyone we just batched (so they re-enter queue)
	var newSched, stillUn int
	if phase == PhaseBatch {
		newSched, stillUn = pl.countNewAndUnscheduledFromBatch(out.Placements, batchedPods)
		pl.activateBatchedPods(batchedPods, 0)
	}

	res := &FlowResult{
		PlanID:           ap.ID,
		Nominated:        out.NominatedNode,
		BatchSize:        len(batchedPods),
		Moves:            len(plan.Moves),
		Evicts:           len(plan.Evicts),
		NewScheduled:     newSched,
		StillUnscheduled: stillUn,
		SolverStatus:     out.Status,
		TotalDuration:    time.Since(start),
		SolverDuration:   solverDur,
	}

	klog.InfoS(string(phase)+": plan execution finished",
		"planID", res.PlanID,
		"nominated", res.Nominated,
		"batchSize", res.BatchSize,
		"moves", res.Moves,
		"evicts", res.Evicts,
		"newScheduled", res.NewScheduled,
		"stillUnscheduled", res.StillUnscheduled,
		"solverStatus", res.SolverStatus,
		"totalDuration", res.TotalDuration,
		"solverDuration", res.SolverDuration,
	)

	// If nothing got newly scheduled we can complete immediately (no tail effects)
	if phase == PhaseBatch && newSched == 0 {
		pl.onPlanSettled()
	}

	return res, nil
}
