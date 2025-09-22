// errors.go

package mycrossnodepreemption

import "errors"

var (
	ErrNoSolverEnabled     = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress    = errors.New(InfoActivePlanInProgress)
	ErrSolverFailed        = errors.New(InfoSolverFailed)
	ErrRegisterPlan        = errors.New(InfoRegisterPlanFailed)
	ErrNoImprovement       = errors.New(InfoNoImprovement)
	ErrNoOptimalOrFeasible = errors.New(InfoNoOptimalOrFeasible)
	ErrNoop                = errors.New(InfoNoop)
	ErrNoPlanProvided      = errors.New(InfoNoPlanProvided)
)
