// errors.go

package mycrossnodepreemption

import "errors"

var (
	ErrNoSolverEnabled                  = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress                 = errors.New(InfoActivePlanInProgress)
	ErrRegisterPlan                     = errors.New(InfoRegisterPlanFailed)
	ErrPlanExecutionFailed              = errors.New(InfoPlanExecutionFailed)
	ErrNoImprovingSolutionFromAnySolver = errors.New(InfoNoImprovingSolutionFromAnySolver)
	ErrNoPendingPodsToSchedule          = errors.New(InfoNoPendingPodsToSchedule)
	ErrNoPendingPods                    = errors.New(InfoNoPendingPods)
	ErrNoPlanProvided                   = errors.New(InfoNoPlanProvided)
	ErrNoUsableNodes                    = errors.New(InfoNoUsableNodes)
)
