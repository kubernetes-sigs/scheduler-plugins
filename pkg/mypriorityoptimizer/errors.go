// errors.go
package mypriorityoptimizer

import "errors"

var (
	ErrNoSolverEnabled                     = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress                    = errors.New(InfoActivePlanInProgress)
	ErrOptimizationInProgress              = errors.New(InfoOptimizationInProgress)
	ErrPlanRegistration                    = errors.New(InfoPlanRegistrationFailed)
	ErrPlanActivationFailed                = errors.New(InfoPlanActivationFailed)
	ErrNoImprovingSolutionFromAnySolver    = errors.New(InfoNoImprovingSolutionFromAnySolver)
	ErrNoPendingPodsScheduled              = errors.New(InfoNoPendingPodsScheduled)
	ErrNoPendingPods                       = errors.New(InfoNoPendingPods)
	ErrNoPlanProvided                      = errors.New(InfoNoPlanProvided)
	ErrNoUsableNodes                       = errors.New(InfoNoUsableNodes)
	ErrInformersCanceledOrTimedOut         = errors.New(InfoInformersCanceledOrTimedOut)
	ErrWaitForUsableNodeCanceledOrTimedOut = errors.New(InfoWaitForUsableNodeCanceledOrTimedOut)
	ErrPlanNotApplicable                   = errors.New(InfoPlanNotApplicable)
	ErrSolverSolutionNotUsable             = errors.New(InfoSolverSolutionNotUsable)
	ErrSolverSolutionNotBetterThanBaseline = errors.New(InfoSolverSolutionNotBetterThanBaseline)
	ErrFailedToListNodes                   = errors.New(InfoFailedToListNodes)
	ErrFailedToListPods                    = errors.New(InfoFailedToListPods)
	ErrFailedToBuildSolverInput            = errors.New(InfoFailedToBuildSolverInput)
)
