// single_flow.go
package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type singleResult struct {
	nominated      string
	planID         string
	moved, evicted int
	solverStatus   string
	totalDur       time.Duration
	solverDur      time.Duration
}

// runSingleFlow encapsulates the common logic used by PreEnqueue/PostFilter when
// running the "every preemptor" optimization for a single pending pod.
func (pl *MyCrossNodePreemption) runSingleFlow(ctx context.Context, pod *v1.Pod, p Phase) (*singleResult, error) {
	if !pl.tryEnterActive() {
		return nil, ErrActiveInProgress // simple sentinel; define in errors.go if you like
	}
	start := time.Now()
	ctxSolve, cancel := context.WithTimeout(ctx, SolverTimeout)
	defer cancel()

	out, err := pl.solve(ctxSolve, SolveSingle, pod, nil, SolverTimeout)
	solverDur := time.Since(start)

	// If solver failed or no nomination → leave Active and surface an error
	if err != nil || out == nil || out.NominatedNode == "" {
		pl.leaveActive()
		if err == nil {
			err = ErrNoNomination // another small sentinel
		}
		return nil, err
	}

	plan, ap, err := pl.registerPlan(ctx, out, pod)
	if err != nil {
		pl.Blocked.AddPod(pod) // safe to treat as blocked so it retries later
		pl.leaveActive()
		return nil, err
	}

	// Execute only when there are ops
	_ = pl.executePlanIfOps(ctx, plan)

	res := &singleResult{
		nominated:    out.NominatedNode,
		planID:       ap.ID,
		moved:        len(plan.Moves),
		evicted:      len(plan.Evicts),
		solverStatus: out.Status,
		totalDur:     time.Since(start),
		solverDur:    solverDur,
	}

	// Phase-specific logging (kept here so callers stay tiny)
	klog.InfoS(string(p)+": plan execution finished",
		"solverStatus", res.solverStatus,
		"pod", klog.KObj(pod),
		"node", res.nominated,
		"planID", res.planID,
		"moved", res.moved,
		"evicted", res.evicted,
		string(p)+"Duration", res.totalDur,
		"solverDuration", res.solverDur,
	)

	return res, nil
}
