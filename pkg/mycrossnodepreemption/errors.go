// errors.go

package mycrossnodepreemption

import "errors"

var (
	ErrNoSolverEnabled     = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress    = errors.New(InfoActivePlanInProgress)
	ErrRegisterPlan        = errors.New(InfoRegisterPlanFailed)
	ErrPlanExecutionFailed = errors.New(InfoPlanExecutionFailed)
	ErrNoSolverSolution    = errors.New(InfoNoSolverSolution)
	ErrNoop                = errors.New(InfoNoop)
	ErrNoPlanProvided      = errors.New(InfoNoPlanProvided)
	ErrNoUsableNodes       = errors.New(InfoNoUsableNodes)
)
