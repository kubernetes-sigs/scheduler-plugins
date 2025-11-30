// errors.go

package mypriorityoptimizer

import "errors"

var (
	ErrNoSolverEnabled                  = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress                 = errors.New(InfoActivePlanInProgress)
	ErrPlanRegistration                 = errors.New(InfoPlanRegistrationFailed)
	ErrPlanActivationFailed             = errors.New(InfoPlanActivationFailed)
	ErrNoImprovingSolutionFromAnySolver = errors.New(InfoNoImprovingSolutionFromAnySolver)
	ErrNoPendingPodsToSchedule          = errors.New(InfoNoPendingPodsToSchedule)
	ErrNoPendingPods                    = errors.New(InfoNoPendingPods)
	ErrNoPlanProvided                   = errors.New(InfoNoPlanProvided)
	ErrNoUsableNodes                    = errors.New(InfoNoUsableNodes)
)
