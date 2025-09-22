// errors.go

package mycrossnodepreemption

import "errors"

var (
	ErrNoSolverEnabled     = errors.New(InfoNoSolverEnabled)
	ErrActiveInProgress    = errors.New(InfoActivePlanInProgress)
	ErrRegisterPlan        = errors.New(InfoRegisterPlanFailed)
	ErrNoOptimalOrFeasible = errors.New(InfoNoOptimalOrFeasible)
	ErrNoop                = errors.New(InfoNoop)
	ErrNoPlanProvided      = errors.New(InfoNoPlanProvided)
)
