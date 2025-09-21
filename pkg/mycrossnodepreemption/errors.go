// errors.go

package mycrossnodepreemption

import "errors"

var (
	ErrNoSolverEnabled     = errors.New("no solver enabled")
	ErrActiveInProgress    = errors.New("active plan in progress")
	ErrSolver              = errors.New("solver failed")
	ErrRegisterPlan        = errors.New("failed to register plan")
	ErrDigestMismatch      = errors.New("cluster changed since solve")
	ErrNoImprovement       = errors.New("no improvement")
	ErrNoNomination        = errors.New("no node nominated for preemption")
	ErrNoOptimalOrFeasible = errors.New("no optimal or feasible solution")
	ErrNoop                = errors.New("no operation")
)
